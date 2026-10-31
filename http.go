package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/server"
)

type ServerConfig struct {
	MasterToken string
	ListenAddr  string
	BaseURL     string
}

// Router builds the complete HTTP handler including API, token management, prompt hydration, SSE MCP, and embedded UI.
func BuildRouter(
	cfg ServerConfig,
	tokenStore TokenStore,
	memStore MemoryStore,
	registry *SessionRegistry,
) http.Handler {
	mux := http.NewServeMux()

	// 1. MCP Setup
	mcpServer := BuildMCPServer(memStore, registry)
	sseServer := server.NewSSEServer(
		mcpServer,
		server.WithSSEEndpoint("/sse"),
		server.WithMessageEndpoint("/message"),
		server.WithAppendQueryToMessageEndpoint(),
		server.WithBaseURL(cfg.BaseURL),
		server.WithSSEDisableLocalhostProtection(true),
		server.WithSSEContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			sessionID := r.URL.Query().Get("sessionId")
			if sessionID != "" {
				if t := registry.Get(sessionID); t != nil {
					ctx = WithAgentToken(ctx, t)
				}
			}
			return ctx
		}),
	)

	streamableServer := server.NewStreamableHTTPServer(
		mcpServer,
		server.WithDisableLocalhostProtection(true),
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			tokenStr := extractBearerToken(r)
			if tokenStr == "" {
				tokenStr = r.URL.Query().Get("token")
			}
			if tokenStr != "" {
				if agentTok, err := tokenStore.GetToken(ctx, tokenStr); err == nil && !agentTok.Revoked {
					ctx = WithAgentToken(ctx, agentTok)
				}
			}
			return ctx
		}),
	)

	// Health endpoint
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "healthy", "service": "mem"})
	})
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "healthy", "service": "mem"})
	})

	// Master Auth Middleware (checks Bearer header, X-Master-Token, query param, or Cookie)
	masterAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			token := extractMasterToken(r)
			if !constantTimeCompare(token, cfg.MasterToken) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized: invalid master token"})
				return
			}
			next(w, r)
		}
	}

	// 2. Token Management API (Authenticated via Master Token)
	// POST /api/tokens - Create Token
	mux.HandleFunc("POST /api/tokens", masterAuth(func(w http.ResponseWriter, r *http.Request) {
		var req AgentToken
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request JSON: " + err.Error()})
			return
		}
		created, err := tokenStore.CreateToken(r.Context(), req)
		if err != nil {
			if errors.Is(err, ErrTokenExists) {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "token already exists"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, created)
	}))

	// GET /api/tokens - List Tokens
	mux.HandleFunc("GET /api/tokens", masterAuth(func(w http.ResponseWriter, r *http.Request) {
		agentID := r.URL.Query().Get("agent_id")
		includeRevoked := r.URL.Query().Get("include_revoked") == "true"
		tokens, err := tokenStore.ListTokens(r.Context(), agentID, includeRevoked)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if tokens == nil {
			tokens = []AgentToken{}
		}
		writeJSON(w, http.StatusOK, tokens)
	}))

	// GET /api/tokens/{token} - Get Token
	mux.HandleFunc("GET /api/tokens/{token}", masterAuth(func(w http.ResponseWriter, r *http.Request) {
		tokenStr := r.PathValue("token")
		tok, err := tokenStore.GetToken(r.Context(), tokenStr)
		if err != nil {
			if errors.Is(err, ErrTokenNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "token not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, tok)
	}))

	// PUT & PATCH /api/tokens/{token} - Update Token
	updateTokenHandler := masterAuth(func(w http.ResponseWriter, r *http.Request) {
		tokenStr := r.PathValue("token")
		var update AgentTokenUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request JSON: " + err.Error()})
			return
		}
		updated, err := tokenStore.UpdateToken(r.Context(), tokenStr, update)
		if err != nil {
			if errors.Is(err, ErrTokenNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "token not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, updated)
	})
	mux.HandleFunc("PUT /api/tokens/{token}", updateTokenHandler)
	mux.HandleFunc("PATCH /api/tokens/{token}", updateTokenHandler)

	// DELETE /api/tokens/{token} - Revoke or Delete Token
	mux.HandleFunc("DELETE /api/tokens/{token}", masterAuth(func(w http.ResponseWriter, r *http.Request) {
		tokenStr := r.PathValue("token")
		permanent := r.URL.Query().Get("permanent") == "true"

		if permanent {
			if err := tokenStore.DeleteToken(r.Context(), tokenStr); err != nil {
				if errors.Is(err, ErrTokenNotFound) {
					writeJSON(w, http.StatusNotFound, map[string]string{"error": "token not found"})
					return
				}
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
			return
		}

		if err := tokenStore.RevokeToken(r.Context(), tokenStr); err != nil {
			if errors.Is(err, ErrTokenNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "token not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
	}))

	// 3. Bucket Management API (Master Auth)
	// GET /api/buckets - List all buckets with statistics
	mux.HandleFunc("GET /api/buckets", masterAuth(func(w http.ResponseWriter, r *http.Request) {
		summaries, err := memStore.ListBuckets(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if summaries == nil {
			summaries = []BucketSummary{}
		}
		writeJSON(w, http.StatusOK, summaries)
	}))

	// DELETE /api/buckets/{bucket} - Empty bucket
	mux.HandleFunc("DELETE /api/buckets/{bucket}", masterAuth(func(w http.ResponseWriter, r *http.Request) {
		bucket := r.PathValue("bucket")
		permanent := r.URL.Query().Get("permanent") == "true"
		affected, err := memStore.EmptyBucket(r.Context(), bucket, permanent)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":   "emptied",
			"bucket":   bucket,
			"affected": affected,
		})
	}))

	// 4. Memory Entry Management API (Master Auth)
	// GET /api/entries & GET /api/buckets/{bucket}/entries
	listEntriesHandler := masterAuth(func(w http.ResponseWriter, r *http.Request) {
		bucket := r.PathValue("bucket")
		if bucket == "" {
			bucket = r.URL.Query().Get("bucket")
		}
		tier := r.URL.Query().Get("tier")
		q := r.URL.Query().Get("q")
		includeSuperseded := r.URL.Query().Get("include_superseded") == "true"

		limit := 50
		if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
			limit = l
		}
		offset := 0
		if o, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && o >= 0 {
			offset = o
		}

		entries, total, err := memStore.ListEntries(r.Context(), bucket, tier, q, includeSuperseded, limit, offset)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if entries == nil {
			entries = []MemoryDetail{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"entries": entries,
			"total":   total,
		})
	})
	mux.HandleFunc("GET /api/entries", listEntriesHandler)
	mux.HandleFunc("GET /api/buckets/{bucket}/entries", listEntriesHandler)

	// GET /api/entries/{id}
	mux.HandleFunc("GET /api/entries/{id}", masterAuth(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid entry id"})
			return
		}
		entry, err := memStore.GetEntry(r.Context(), id)
		if err != nil {
			if errors.Is(err, ErrMemoryNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "entry not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, entry)
	}))

	// POST /api/entries & POST /api/buckets/{bucket}/entries - Add entry
	addEntryHandler := masterAuth(func(w http.ResponseWriter, r *http.Request) {
		type AddEntryRequest struct {
			Bucket   string         `json:"bucket"`
			Tier     string         `json:"tier"`
			Subject  string         `json:"subject"`
			Content  string         `json:"content"`
			Metadata map[string]any `json:"metadata"`
		}
		var req AddEntryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}

		bucket := r.PathValue("bucket")
		if bucket != "" {
			req.Bucket = bucket
		}

		if req.Bucket == "" || req.Subject == "" || req.Content == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bucket, subject, and content are required"})
			return
		}

		entry, err := memStore.AddEntry(r.Context(), req.Bucket, req.Tier, req.Subject, req.Content, req.Metadata)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, entry)
	})
	mux.HandleFunc("POST /api/entries", addEntryHandler)
	mux.HandleFunc("POST /api/buckets/{bucket}/entries", addEntryHandler)

	// PUT & PATCH /api/entries/{id} - Edit entry
	editEntryHandler := masterAuth(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid entry id"})
			return
		}

		type EditEntryRequest struct {
			Bucket   string         `json:"bucket"`
			Tier     string         `json:"tier"`
			Subject  string         `json:"subject"`
			Content  string         `json:"content"`
			Metadata map[string]any `json:"metadata"`
		}
		var req EditEntryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}

		entry, err := memStore.UpdateEntry(r.Context(), id, req.Bucket, req.Tier, req.Subject, req.Content, req.Metadata)
		if err != nil {
			if errors.Is(err, ErrMemoryNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "entry not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, entry)
	})
	mux.HandleFunc("PUT /api/entries/{id}", editEntryHandler)
	mux.HandleFunc("PATCH /api/entries/{id}", editEntryHandler)

	// DELETE /api/entries/{id} - Remove single entry
	mux.HandleFunc("DELETE /api/entries/{id}", masterAuth(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid entry id"})
			return
		}

		permanent := r.URL.Query().Get("permanent") == "true"
		if err := memStore.DeleteEntry(r.Context(), id, permanent); err != nil {
			if errors.Is(err, ErrMemoryNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "entry not found"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "id": id})
	}))

	// 5. Prompt-Time Extending Operation (Hydrate Prompt Context)
	hydrateHandler := func(w http.ResponseWriter, r *http.Request) {
		tokenStr := extractMasterToken(r)

		var (
			allowedBuckets []string
			isMaster       = false
		)

		if tokenStr != "" && constantTimeCompare(tokenStr, cfg.MasterToken) {
			isMaster = true
		} else if tokenStr != "" {
			agentTok, err := tokenStore.GetToken(r.Context(), tokenStr)
			if err != nil || agentTok.Revoked {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized: invalid or revoked token"})
				return
			}
			allowedBuckets = agentTok.ReadBuckets
		} else {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized: authentication required"})
			return
		}

		type HydrateRequest struct {
			Query   string   `json:"query"`
			Buckets []string `json:"buckets"`
			TopK    int      `json:"top_k"`
		}

		var req HydrateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request JSON: " + err.Error()})
			return
		}

		if req.TopK <= 0 {
			req.TopK = 5
		}

		targetBuckets := allowedBuckets
		if isMaster {
			targetBuckets = req.Buckets
		} else if len(req.Buckets) > 0 {
			for _, b := range req.Buckets {
				if !slices.Contains(allowedBuckets, b) {
					writeJSON(w, http.StatusForbidden, map[string]string{
						"error": fmt.Sprintf("permission denied: agent cannot access bucket %q", b),
					})
					return
				}
			}
			targetBuckets = req.Buckets
		}

		core, recalled, err := memStore.HydratePromptContext(r.Context(), targetBuckets, req.Query, req.TopK)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		if core == nil {
			core = []MemoryRecord{}
		}
		if recalled == nil {
			recalled = []MemoryRecord{}
		}

		promptContext := memStore.FormatPromptContext(core, recalled)

		writeJSON(w, http.StatusOK, map[string]any{
			"core":           core,
			"recalled":       recalled,
			"prompt_context": promptContext,
		})
	}
	mux.HandleFunc("POST /api/prompt/hydrate", hydrateHandler)
	mux.HandleFunc("POST /api/hydrate", hydrateHandler)

	// 6. MCP Endpoints (Dynamic Authentication for Agents - SSE & Streamable HTTP)
	mcpAuthHandler := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS, HEAD")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, *")
				w.WriteHeader(http.StatusNoContent)
				return
			}

			tokenStr := extractBearerToken(r)
			if tokenStr == "" {
				tokenStr = r.URL.Query().Get("token")
			}

			if tokenStr == "" {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized: agent token required via Authorization header or ?token="})
				return
			}

			agentTok, err := tokenStore.GetToken(r.Context(), tokenStr)
			if err != nil {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized: invalid agent token"})
				return
			}
			if agentTok.Revoked {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized: agent token is revoked"})
				return
			}

			ctx := WithAgentToken(r.Context(), agentTok)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}

	sseHandler := mcpAuthHandler(sseServer.SSEHandler())
	msgHandler := sseServer.MessageHandler()
	streamableHandler := mcpAuthHandler(streamableServer)

	// SSE Transport (Legacy 2024 MCP)
	mux.Handle("GET /sse", sseHandler)
	mux.Handle("POST /message", msgHandler)
	mux.Handle("GET /mcp/sse", sseHandler)
	mux.Handle("POST /mcp/message", msgHandler)

	// Streamable HTTP Transport (Modern 2025+ MCP)
	mux.Handle("POST /sse", streamableHandler)
	mux.Handle("POST /mcp", streamableHandler)
	mux.Handle("GET /mcp", streamableHandler)
	mux.Handle("DELETE /mcp", streamableHandler)
	mux.Handle("POST /mcp/", streamableHandler)
	mux.Handle("GET /mcp/", streamableHandler)
	mux.Handle("DELETE /mcp/", streamableHandler)

	// 7. Embedded Web UI (/ui, /ui/<token>, /ui?token=<token>, SPA routing)
	RegisterUIHandlers(mux, cfg)

	// Redirect root / to /ui/
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})

	return mux
}

func extractBearerToken(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return ""
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

func extractMasterToken(r *http.Request) string {
	if tok := extractBearerToken(r); tok != "" {
		return tok
	}
	if tok := r.Header.Get("X-Master-Token"); tok != "" {
		return strings.TrimSpace(tok)
	}
	if tok := r.URL.Query().Get("token"); tok != "" {
		return strings.TrimSpace(tok)
	}
	if c, err := r.Cookie("mem_master_token"); err == nil && c.Value != "" {
		return strings.TrimSpace(c.Value)
	}
	return ""
}

func constantTimeCompare(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
