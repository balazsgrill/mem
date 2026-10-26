package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/mark3labs/mcp-go/server"
)

type ServerConfig struct {
	MasterToken string
	ListenAddr  string
	BaseURL     string
}

// Router builds the complete HTTP handler including API, token management, prompt hydration, and SSE MCP.
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

	// Health endpoint
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "healthy", "service": "mem"})
	})
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "healthy", "service": "mem"})
	})

	// 2. Token Management API (Authenticated via Master Token)
	masterAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			token := extractBearerToken(r)
			if token == "" {
				token = r.Header.Get("X-Master-Token")
			}
			if !constantTimeCompare(token, cfg.MasterToken) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized: invalid master token"})
				return
			}
			next(w, r)
		}
	}

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

	// 3. Prompt-Time Extending Operation (Hydrate Prompt Context)
	// Accessible via either Agent Token or Master Token
	hydrateHandler := func(w http.ResponseWriter, r *http.Request) {
		tokenStr := extractBearerToken(r)
		if tokenStr == "" {
			tokenStr = r.URL.Query().Get("token")
		}

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

		// Bucket filtering
		targetBuckets := allowedBuckets
		if isMaster {
			targetBuckets = req.Buckets
		} else if len(req.Buckets) > 0 {
			// Ensure requested buckets are within agent's read buckets
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

	// 4. SSE MCP Endpoints (Dynamic Authentication for Agents)
	sseAuthHandler := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

			// Pass authenticated agent token in context
			ctx := WithAgentToken(r.Context(), agentTok)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}

	sseHandler := sseAuthHandler(sseServer.SSEHandler())
	msgHandler := sseServer.MessageHandler()

	mux.Handle("GET /sse", sseHandler)
	mux.Handle("POST /message", msgHandler)
	mux.Handle("GET /mcp/sse", sseHandler)
	mux.Handle("POST /mcp/message", msgHandler)

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
