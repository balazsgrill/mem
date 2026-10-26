package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type agentTokenContextKey struct{}

func WithAgentToken(ctx context.Context, token *AgentToken) context.Context {
	return context.WithValue(ctx, agentTokenContextKey{}, token)
}

func AgentTokenFromContext(ctx context.Context) *AgentToken {
	if val := ctx.Value(agentTokenContextKey{}); val != nil {
		if t, ok := val.(*AgentToken); ok {
			return t
		}
	}
	return nil
}

type SessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*AgentToken
}

func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{
		sessions: make(map[string]*AgentToken),
	}
}

func (r *SessionRegistry) Register(sessionID string, token *AgentToken) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[sessionID] = token
}

func (r *SessionRegistry) Unregister(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, sessionID)
}

func (r *SessionRegistry) Get(sessionID string) *AgentToken {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessions[sessionID]
}

// BuildMCPServer creates and configures the MCP server with memory tools.
func BuildMCPServer(memStore MemoryStore, registry *SessionRegistry) *server.MCPServer {
	hooks := &server.Hooks{}
	hooks.AddOnRegisterSession(func(ctx context.Context, session server.ClientSession) {
		if token := AgentTokenFromContext(ctx); token != nil {
			registry.Register(session.SessionID(), token)
		}
	})
	hooks.AddOnUnregisterSession(func(ctx context.Context, session server.ClientSession) {
		registry.Unregister(session.SessionID())
	})

	mcpServer := server.NewMCPServer(
		"agent-memory",
		"1.0.0",
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true),
		server.WithHooks(hooks),
	)

	// Tool 1: search_memory
	searchTool := mcp.NewTool("search_memory",
		mcp.WithDescription("Search semantic memories using hybrid vector and full-text search across allowed buckets"),
		mcp.WithString("query",
			mcp.Description("Search query or question"),
			mcp.Required(),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of memories to return (default 5, max 50)"),
		),
		mcp.WithString("bucket",
			mcp.Description("Optional specific bucket to search in. Must be in agent's read_buckets"),
		),
	)
	mcpServer.AddTool(searchTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		token := resolveAgentToken(ctx, registry)
		if token == nil {
			return mcp.NewToolResultError("unauthorized: missing or invalid agent session credentials"), nil
		}
		if token.Revoked {
			return mcp.NewToolResultError("unauthorized: agent token has been revoked"), nil
		}

		args := req.GetArguments()
		query := getStringArg(args, "query", "")
		if strings.TrimSpace(query) == "" {
			return mcp.NewToolResultError("query argument cannot be empty"), nil
		}

		limit := getIntArg(args, "limit", 5)
		if limit <= 0 {
			limit = 5
		}
		if limit > 50 {
			limit = 50
		}

		bucket := strings.TrimSpace(getStringArg(args, "bucket", ""))
		var allowed []string
		if bucket != "" {
			if !slices.Contains(token.ReadBuckets, bucket) {
				return mcp.NewToolResultErrorf("permission denied: agent cannot read from bucket %q", bucket), nil
			}
			allowed = []string{bucket}
		} else {
			allowed = token.ReadBuckets
		}

		records, err := memStore.HybridSearch(ctx, allowed, query, limit)
		if err != nil {
			return mcp.NewToolResultErrorf("memory search failed: %v", err), nil
		}

		data, err := json.MarshalIndent(records, "", "  ")
		if err != nil {
			return mcp.NewToolResultErrorf("failed encoding search results: %v", err), nil
		}

		return mcp.NewToolResultText(string(data)), nil
	})

	// Tool 2: remember_fact
	rememberTool := mcp.NewTool("remember_fact",
		mcp.WithDescription("Store a new fact or memory, optionally superseding an existing memory ID"),
		mcp.WithString("bucket",
			mcp.Description("Target bucket to write into. Must be in agent's write_buckets"),
			mcp.Required(),
		),
		mcp.WithString("subject",
			mcp.Description("Topic, entity key, or subject"),
			mcp.Required(),
		),
		mcp.WithString("content",
			mcp.Description("Distilled fact or information to remember"),
			mcp.Required(),
		),
		mcp.WithString("tier",
			mcp.Description("Memory tier: 'core' (pinned in prompt hydration) or 'semantic' (searchable). Default: 'semantic'"),
			mcp.Enum("core", "semantic"),
		),
		mcp.WithNumber("supersedes_id",
			mcp.Description("Optional memory ID that this new fact replaces"),
		),
	)
	mcpServer.AddTool(rememberTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		token := resolveAgentToken(ctx, registry)
		if token == nil {
			return mcp.NewToolResultError("unauthorized: missing or invalid agent session credentials"), nil
		}
		if token.Revoked {
			return mcp.NewToolResultError("unauthorized: agent token has been revoked"), nil
		}

		args := req.GetArguments()
		bucket := strings.TrimSpace(getStringArg(args, "bucket", ""))
		subject := strings.TrimSpace(getStringArg(args, "subject", ""))
		content := strings.TrimSpace(getStringArg(args, "content", ""))
		tier := strings.TrimSpace(getStringArg(args, "tier", "semantic"))
		supersedesID := getInt64PtrArg(args, "supersedes_id")

		if bucket == "" {
			return mcp.NewToolResultError("bucket argument is required"), nil
		}
		if subject == "" {
			return mcp.NewToolResultError("subject argument is required"), nil
		}
		if content == "" {
			return mcp.NewToolResultError("content argument is required"), nil
		}

		if !slices.Contains(token.WriteBuckets, bucket) {
			return mcp.NewToolResultErrorf("permission denied: agent cannot write to bucket %q", bucket), nil
		}

		newID, err := memStore.RememberFact(ctx, token.WriteBuckets, bucket, tier, subject, content, nil, supersedesID)
		if err != nil {
			return mcp.NewToolResultErrorf("failed remembering fact: %v", err), nil
		}

		msg := fmt.Sprintf("Remembered fact successfully with ID %d in bucket %q (tier: %s)", newID, bucket, tier)
		if supersedesID != nil && *supersedesID > 0 {
			msg += fmt.Sprintf(", superseding memory ID %d", *supersedesID)
		}
		return mcp.NewToolResultText(msg), nil
	})

	// Tool 3: get_core_memories
	coreTool := mcp.NewTool("get_core_memories",
		mcp.WithDescription("Retrieve all active pinned core facts for the agent's allowed read buckets"),
		mcp.WithString("bucket",
			mcp.Description("Optional specific bucket to filter core facts. Must be in agent's read_buckets"),
		),
	)
	mcpServer.AddTool(coreTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		token := resolveAgentToken(ctx, registry)
		if token == nil {
			return mcp.NewToolResultError("unauthorized: missing or invalid agent session credentials"), nil
		}
		if token.Revoked {
			return mcp.NewToolResultError("unauthorized: agent token has been revoked"), nil
		}

		args := req.GetArguments()
		bucket := strings.TrimSpace(getStringArg(args, "bucket", ""))
		var allowed []string
		if bucket != "" {
			if !slices.Contains(token.ReadBuckets, bucket) {
				return mcp.NewToolResultErrorf("permission denied: agent cannot read from bucket %q", bucket), nil
			}
			allowed = []string{bucket}
		} else {
			allowed = token.ReadBuckets
		}

		records, err := memStore.GetCoreMemories(ctx, allowed)
		if err != nil {
			return mcp.NewToolResultErrorf("failed retrieving core memories: %v", err), nil
		}

		data, err := json.MarshalIndent(records, "", "  ")
		if err != nil {
			return mcp.NewToolResultErrorf("failed encoding core memories: %v", err), nil
		}

		return mcp.NewToolResultText(string(data)), nil
	})

	// Tool 4: forget_fact
	forgetTool := mcp.NewTool("forget_fact",
		mcp.WithDescription("Invalidate or forget an active memory by its ID. Must be in a writable bucket"),
		mcp.WithNumber("id",
			mcp.Description("The memory ID to invalidate"),
			mcp.Required(),
		),
	)
	mcpServer.AddTool(forgetTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		token := resolveAgentToken(ctx, registry)
		if token == nil {
			return mcp.NewToolResultError("unauthorized: missing or invalid agent session credentials"), nil
		}
		if token.Revoked {
			return mcp.NewToolResultError("unauthorized: agent token has been revoked"), nil
		}

		args := req.GetArguments()
		id := getInt64Arg(args, "id", 0)
		if id <= 0 {
			return mcp.NewToolResultError("valid positive id argument is required"), nil
		}

		if err := memStore.InvalidateFact(ctx, token.WriteBuckets, id); err != nil {
			return mcp.NewToolResultErrorf("failed to invalidate memory: %v", err), nil
		}

		return mcp.NewToolResultText(fmt.Sprintf("Memory fact %d successfully invalidated", id)), nil
	})

	return mcpServer
}

func resolveAgentToken(ctx context.Context, registry *SessionRegistry) *AgentToken {
	if token := AgentTokenFromContext(ctx); token != nil {
		return token
	}
	session := server.ClientSessionFromContext(ctx)
	if session != nil {
		return registry.Get(session.SessionID())
	}
	return nil
}

func getStringArg(args map[string]any, key, def string) string {
	if val, ok := args[key]; ok {
		if s, ok := val.(string); ok {
			return s
		}
	}
	return def
}

func getIntArg(args map[string]any, key string, def int) int {
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case float64:
			return int(v)
		case int:
			return v
		case int64:
			return int(v)
		}
	}
	return def
}

func getInt64Arg(args map[string]any, key string, def int64) int64 {
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case float64:
			return int64(v)
		case int:
			return int64(v)
		case int64:
			return v
		}
	}
	return def
}

func getInt64PtrArg(args map[string]any, key string) *int64 {
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case float64:
			i := int64(v)
			return &i
		case int64:
			return &v
		case int:
			i := int64(v)
			return &i
		}
	}
	return nil
}
