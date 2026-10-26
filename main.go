package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func getEnv(key, def string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	if val := os.Getenv(key); val != "" {
		val = strings.ToLower(val)
		return val == "true" || val == "1" || val == "yes"
	}
	return def
}

func main() {
	var (
		dbURL          = flag.String("db", getEnv("DATABASE_URL", ""), "PostgreSQL connection URL (e.g. postgres://user:pass@localhost:5432/mem?sslmode=disable)")
		listenAddr     = flag.String("addr", getEnv("LISTEN_ADDR", ":8080"), "HTTP server listen address")
		masterToken    = flag.String("master-token", getEnv("MASTER_TOKEN", getEnv("MEM_MASTER_TOKEN", "")), "Master authentication token for admin API")
		baseURL        = flag.String("base-url", getEnv("BASE_URL", ""), "Base URL for the server (optional, e.g. http://localhost:8080)")
		autoMigrate    = flag.Bool("auto-migrate", getEnvBool("AUTO_MIGRATE", true), "Automatically apply schema migrations on startup")
		embedderType   = flag.String("embedder", getEnv("EMBEDDER_TYPE", ""), "Embedder provider ('openai', 'ollama', or 'mock')")
		openaiKey      = flag.String("openai-key", getEnv("OPENAI_API_KEY", ""), "OpenAI API Key")
		openaiBaseURL  = flag.String("openai-base-url", getEnv("OPENAI_BASE_URL", "https://api.openai.com/v1"), "OpenAI compatible base URL")
		openaiModel    = flag.String("openai-model", getEnv("OPENAI_MODEL", "text-embedding-3-small"), "OpenAI embedding model")
		ollamaBaseURL  = flag.String("ollama-base-url", getEnv("OLLAMA_BASE_URL", "http://localhost:11434"), "Ollama base URL")
		ollamaModel    = flag.String("ollama-model", getEnv("OLLAMA_MODEL", "nomic-embed-text"), "Ollama embedding model")
	)
	flag.Parse()

	// 1. Setup Master Token
	token := *masterToken
	if strings.TrimSpace(token) == "" {
		generated, err := GenerateSecureToken("master_")
		if err != nil {
			log.Fatalf("Failed generating master token: %v", err)
		}
		token = generated
		log.Printf("[AUTH] No MASTER_TOKEN configured. Generated ephemeral master token: %s", token)
	} else {
		log.Printf("[AUTH] Configured master token is active")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 2. Setup Embedder
	var embedder Embedder
	switch strings.ToLower(*embedderType) {
	case "mock":
		embedder = NewMockEmbedder(768)
		log.Println("[EMBEDDER] Using deterministic local mock embedder (768-dim)")
	case "ollama":
		embedder = NewOllamaEmbedder(*ollamaBaseURL, *ollamaModel, 768)
		log.Printf("[EMBEDDER] Using Ollama embedder at %s (model: %s)", *ollamaBaseURL, *ollamaModel)
	case "openai":
		embedder = NewOpenAIEmbedder(*openaiBaseURL, *openaiKey, *openaiModel, 768)
		log.Printf("[EMBEDDER] Using OpenAI embedder at %s (model: %s)", *openaiBaseURL, *openaiModel)
	default:
		if *openaiKey != "" {
			embedder = NewOpenAIEmbedder(*openaiBaseURL, *openaiKey, *openaiModel, 768)
			log.Printf("[EMBEDDER] Auto-selected OpenAI embedder (model: %s)", *openaiModel)
		} else if os.Getenv("OLLAMA_BASE_URL") != "" {
			embedder = NewOllamaEmbedder(*ollamaBaseURL, *ollamaModel, 768)
			log.Printf("[EMBEDDER] Auto-selected Ollama embedder at %s", *ollamaBaseURL)
		} else {
			embedder = NewMockEmbedder(768)
			log.Println("[EMBEDDER] No embedding credentials configured; using deterministic local embedder (768-dim)")
		}
	}

	// 3. Setup Stores (Postgres or In-Memory)
	var (
		tokenStore TokenStore
		memStore   MemoryStore
		pgPool     *pgxpool.Pool
	)

	if strings.TrimSpace(*dbURL) != "" {
		log.Printf("[DB] Connecting to PostgreSQL at %s", sanitizeDBURL(*dbURL))
		poolConfig, err := pgxpool.ParseConfig(*dbURL)
		if err != nil {
			log.Fatalf("[DB] Invalid database connection string: %v", err)
		}
		poolConfig.MaxConns = 25
		poolConfig.MinConns = 2

		pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
		if err != nil {
			log.Fatalf("[DB] Failed connecting to database: %v", err)
		}
		pgPool = pool

		// Verify connection
		pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
		if err := pgPool.Ping(pingCtx); err != nil {
			pingCancel()
			log.Fatalf("[DB] Database ping failed: %v", err)
		}
		pingCancel()
		log.Println("[DB] Successfully connected to PostgreSQL")

		// Run schema migrations if enabled
		if *autoMigrate {
			log.Println("[DB] Running schema migration...")
			if err := EnsureSchema(ctx, pgPool); err != nil {
				log.Fatalf("[DB] Schema migration failed: %v", err)
			}
			log.Println("[DB] Schema migration completed successfully")
		}

		tokenStore = NewDBTokenStore(pgPool)
		memStore = NewStore(pgPool, embedder)
	} else {
		log.Println("[WARN] No DATABASE_URL provided. Running in In-Memory mode (suitable for testing/development).")
		tokenStore = NewInMemoryTokenStore()
		memStore = NewInMemoryStore(embedder)
	}

	// 4. Build HTTP Router & Server
	registry := NewSessionRegistry()
	cfg := ServerConfig{
		MasterToken: token,
		ListenAddr:  *listenAddr,
		BaseURL:     *baseURL,
	}

	router := BuildRouter(cfg, tokenStore, memStore, registry)
	srv := &http.Server{
		Addr:         *listenAddr,
		Handler:      router,
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 5. Graceful Shutdown
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("[SERVER] Starting lightweight agent memory server on %s", *listenAddr)
		log.Printf("[SERVER] - SSE MCP endpoint: http://localhost%s/sse", *listenAddr)
		log.Printf("[SERVER] - Token Admin API: http://localhost%s/api/tokens", *listenAddr)
		log.Printf("[SERVER] - Prompt Hydration API: http://localhost%s/api/prompt/hydrate", *listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[SERVER] HTTP server error: %v", err)
		}
	}()

	<-stopChan
	log.Println("[SERVER] Shutting down gracefully...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[SERVER] Server shutdown error: %v", err)
	}
	if pgPool != nil {
		pgPool.Close()
		log.Println("[DB] Closed database connection pool")
	}

	log.Println("[SERVER] Server stopped.")
}

func sanitizeDBURL(raw string) string {
	if idx := strings.Index(raw, "@"); idx != -1 {
		if schemeIdx := strings.Index(raw, "://"); schemeIdx != -1 {
			return raw[:schemeIdx+3] + "***:***" + raw[idx:]
		}
	}
	return raw
}
