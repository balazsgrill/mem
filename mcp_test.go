package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

type sseClient struct {
	sseURL      string
	token       string
	sessionID   string
	messageURL  string
	resp        *http.Response
	reader      *bufio.Reader
	eventQueue  chan sseEvent
	done        chan struct{}
}

type sseEvent struct {
	EventType string
	Data      string
}

func connectSSE(serverURL, token string) (*sseClient, error) {
	url := fmt.Sprintf("%s/sse", serverURL)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	client := &sseClient{
		sseURL:     url,
		token:      token,
		resp:       resp,
		reader:     bufio.NewReader(resp.Body),
		eventQueue: make(chan sseEvent, 100),
		done:       make(chan struct{}),
	}

	go client.listen()

	// Read initial endpoint event
	select {
	case ev := <-client.eventQueue:
		if ev.EventType != "endpoint" {
			client.close()
			return nil, fmt.Errorf("expected endpoint event, got %s", ev.EventType)
		}
		endpoint := ev.Data
		if strings.HasPrefix(endpoint, "http") {
			client.messageURL = endpoint
		} else {
			client.messageURL = serverURL + endpoint
		}
	case <-time.After(3 * time.Second):
		client.close()
		return nil, fmt.Errorf("timed out waiting for endpoint event")
	}

	return client, nil
}

func (c *sseClient) listen() {
	var currentEvent, currentData strings.Builder
	for {
		select {
		case <-c.done:
			return
		default:
			line, err := c.reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				// Event completed
				if currentEvent.Len() > 0 || currentData.Len() > 0 {
					evType := currentEvent.String()
					if evType == "" {
						evType = "message"
					}
					c.eventQueue <- sseEvent{
						EventType: evType,
						Data:      currentData.String(),
					}
					currentEvent.Reset()
					currentData.Reset()
				}
				continue
			}

			if strings.HasPrefix(line, "event: ") {
				currentEvent.WriteString(strings.TrimPrefix(line, "event: "))
			} else if strings.HasPrefix(line, "data: ") {
				currentData.WriteString(strings.TrimPrefix(line, "data: "))
			}
		}
	}
}

func (c *sseClient) close() {
	select {
	case <-c.done:
	default:
		close(c.done)
		if c.resp != nil && c.resp.Body != nil {
			c.resp.Body.Close()
		}
	}
}

func (c *sseClient) callTool(name string, args map[string]any, requestID int64) (*mcp.CallToolResult, error) {
	rpcReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}
	body, err := json.Marshal(rpcReq)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, c.messageURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	postResp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer postResp.Body.Close()

	if postResp.StatusCode != http.StatusAccepted && postResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("message POST returned status %d", postResp.StatusCode)
	}

	// Wait for response over SSE
	select {
	case ev := <-c.eventQueue:
		type JSONRPCResponse struct {
			JSONRPC string              `json:"jsonrpc"`
			ID      any                 `json:"id"`
			Result  *mcp.CallToolResult `json:"result,omitempty"`
			Error   *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error,omitempty"`
		}
		var rpcResp JSONRPCResponse
		if err := json.Unmarshal([]byte(ev.Data), &rpcResp); err != nil {
			return nil, fmt.Errorf("failed decoding RPC response %s: %w", ev.Data, err)
		}
		if rpcResp.Error != nil {
			return nil, fmt.Errorf("RPC error (%d): %s", rpcResp.Error.Code, rpcResp.Error.Message)
		}
		return rpcResp.Result, nil
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("timeout waiting for tool result over SSE")
	}
}

func TestSSEResidentServerAuth(t *testing.T) {
	handler, _, _, _ := setupTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// 1. Connection without token -> 401
	_, err := connectSSE(ts.URL, "")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 unauthorized when connecting without token, got: %v", err)
	}

	// 2. Connection with invalid token -> 401
	_, err = connectSSE(ts.URL, "invalid-random-token")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 unauthorized when connecting with invalid token, got: %v", err)
	}
}

func TestSSEResidentServerTools(t *testing.T) {
	handler, tokenStore, _, _ := setupTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	ctx := context.Background()

	// Create test agent token
	agentTok, err := tokenStore.CreateToken(ctx, AgentToken{
		AgentID:      "agent_tester",
		ReadBuckets:  []string{"homelab"},
		WriteBuckets: []string{"homelab"},
	})
	if err != nil {
		t.Fatalf("failed creating token: %v", err)
	}

	// Connect SSE client
	client, err := connectSSE(ts.URL, agentTok.Token)
	if err != nil {
		t.Fatalf("failed connecting SSE: %v", err)
	}
	defer client.close()

	// 1. Call remember_fact in allowed bucket "homelab"
	remRes, err := client.callTool("remember_fact", map[string]any{
		"bucket":  "homelab",
		"subject": "Firewall",
		"content": "OPNsense router configured at 192.168.1.1",
		"tier":    "core",
	}, 1)
	if err != nil {
		t.Fatalf("remember_fact call failed: %v", err)
	}
	if remRes.IsError {
		t.Fatalf("expected successful remember_fact, got error result: %v", remRes.Content)
	}

	// 2. Call remember_fact in unauthorized bucket "secret" -> should return error
	unauthRes, err := client.callTool("remember_fact", map[string]any{
		"bucket":  "secret",
		"subject": "Password",
		"content": "Secret admin password",
	}, 2)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !unauthRes.IsError {
		t.Fatalf("expected permission denied error for unauthorized bucket")
	}

	// 3. Call search_memory
	searchRes, err := client.callTool("search_memory", map[string]any{
		"query": "OPNsense router",
		"limit": 5,
	}, 3)
	if err != nil {
		t.Fatalf("search_memory call failed: %v", err)
	}
	if searchRes.IsError {
		t.Fatalf("expected successful search_memory, got error: %v", searchRes.Content)
	}

	// 4. Call get_core_memories
	coreRes, err := client.callTool("get_core_memories", map[string]any{}, 4)
	if err != nil {
		t.Fatalf("get_core_memories call failed: %v", err)
	}
	if coreRes.IsError {
		t.Fatalf("expected successful get_core_memories, got error: %v", coreRes.Content)
	}
	text := ""
	if len(coreRes.Content) > 0 {
		if tc, ok := coreRes.Content[0].(mcp.TextContent); ok {
			text = tc.Text
		}
	}
	if !strings.Contains(text, "Firewall") {
		t.Fatalf("expected core memories to contain 'Firewall', got: %s", text)
	}

	// 5. Call forget_fact
	forgetRes, err := client.callTool("forget_fact", map[string]any{
		"id": 1,
	}, 5)
	if err != nil {
		t.Fatalf("forget_fact call failed: %v", err)
	}
	if forgetRes.IsError {
		t.Fatalf("expected successful forget_fact, got error: %v", forgetRes.Content)
	}
}

func TestParallelMultiAgentSSE(t *testing.T) {
	handler, tokenStore, _, _ := setupTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	ctx := context.Background()

	// Create Agent Alice token
	aliceTok, err := tokenStore.CreateToken(ctx, AgentToken{
		AgentID:      "agent_alice",
		ReadBuckets:  []string{"shared", "alice_bucket"},
		WriteBuckets: []string{"alice_bucket"},
	})
	if err != nil {
		t.Fatalf("failed creating alice token: %v", err)
	}

	// Create Agent Bob token
	bobTok, err := tokenStore.CreateToken(ctx, AgentToken{
		AgentID:      "agent_bob",
		ReadBuckets:  []string{"shared", "bob_bucket"},
		WriteBuckets: []string{"bob_bucket"},
	})
	if err != nil {
		t.Fatalf("failed creating bob token: %v", err)
	}

	// Connect both agents in parallel
	aliceClient, err := connectSSE(ts.URL, aliceTok.Token)
	if err != nil {
		t.Fatalf("failed connecting alice: %v", err)
	}
	defer aliceClient.close()

	bobClient, err := connectSSE(ts.URL, bobTok.Token)
	if err != nil {
		t.Fatalf("failed connecting bob: %v", err)
	}
	defer bobClient.close()

	var wg sync.WaitGroup
	wg.Add(2)

	var aliceErr, bobErr error

	// Alice writes her private secret
	go func() {
		defer wg.Done()
		res, err := aliceClient.callTool("remember_fact", map[string]any{
			"bucket":  "alice_bucket",
			"subject": "AliceSecret",
			"content": "Alice favorite dessert is tiramisu",
			"tier":    "semantic",
		}, 10)
		if err != nil {
			aliceErr = err
			return
		}
		if res.IsError {
			aliceErr = fmt.Errorf("alice remember failed: %v", res.Content)
		}
	}()

	// Bob writes his private secret
	go func() {
		defer wg.Done()
		res, err := bobClient.callTool("remember_fact", map[string]any{
			"bucket":  "bob_bucket",
			"subject": "BobSecret",
			"content": "Bob favorite hobby is sailing",
			"tier":    "semantic",
		}, 20)
		if err != nil {
			bobErr = err
			return
		}
		if res.IsError {
			bobErr = fmt.Errorf("bob remember failed: %v", res.Content)
		}
	}()

	wg.Wait()

	if aliceErr != nil {
		t.Fatalf("alice error during parallel write: %v", aliceErr)
	}
	if bobErr != nil {
		t.Fatalf("bob error during parallel write: %v", bobErr)
	}

	// Now verify isolation:
	// Bob searches for "Alice" -> should get 0 results
	bobSearch, err := bobClient.callTool("search_memory", map[string]any{
		"query": "tiramisu dessert",
	}, 21)
	if err != nil {
		t.Fatalf("bob search failed: %v", err)
	}
	bobText := ""
	if len(bobSearch.Content) > 0 {
		if tc, ok := bobSearch.Content[0].(mcp.TextContent); ok {
			bobText = tc.Text
		}
	}
	if strings.Contains(bobText, "tiramisu") {
		t.Fatalf("Bob should not have access to Alice's bucket, but got: %s", bobText)
	}

	// Alice searches for "tiramisu" -> should find her fact
	aliceSearch, err := aliceClient.callTool("search_memory", map[string]any{
		"query": "tiramisu dessert",
	}, 11)
	if err != nil {
		t.Fatalf("alice search failed: %v", err)
	}
	aliceText := ""
	if len(aliceSearch.Content) > 0 {
		if tc, ok := aliceSearch.Content[0].(mcp.TextContent); ok {
			aliceText = tc.Text
		}
	}
	if !strings.Contains(aliceText, "tiramisu") {
		t.Fatalf("Alice should have found her memory, got: %s", aliceText)
	}
}

func TestOfficialMCPClient(t *testing.T) {
	handler, tokenStore, _, _ := setupTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agentTok, err := tokenStore.CreateToken(ctx, AgentToken{
		AgentID:      "official_client",
		ReadBuckets:  []string{"homelab"},
		WriteBuckets: []string{"homelab"},
	})
	if err != nil {
		t.Fatalf("failed creating token: %v", err)
	}

	// Connect using SSEMCPClient with header auth
	mcpClient, err := client.NewSSEMCPClient(
		ts.URL+"/sse",
		transport.WithHeaders(map[string]string{
			"Authorization": "Bearer " + agentTok.Token,
		}),
	)
	if err != nil {
		t.Fatalf("failed creating client: %v", err)
	}
	defer mcpClient.Close()

	if err := mcpClient.Start(ctx); err != nil {
		t.Fatalf("failed to start client: %v", err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}

	initRes, err := mcpClient.Initialize(ctx, initReq)
	if err != nil {
		t.Fatalf("failed to initialize: %v", err)
	}
	t.Logf("Initialized server: %s %s", initRes.ServerInfo.Name, initRes.ServerInfo.Version)

	toolsRes, err := mcpClient.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("failed to list tools: %v", err)
	}
	if len(toolsRes.Tools) == 0 {
		t.Fatalf("expected tools, got 0")
	}
	t.Logf("Got %d tools", len(toolsRes.Tools))
}

func TestStreamableHTTPClient(t *testing.T) {
	handler, tokenStore, _, _ := setupTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agentTok, err := tokenStore.CreateToken(ctx, AgentToken{
		AgentID:      "streamable_client",
		ReadBuckets:  []string{"homelab"},
		WriteBuckets: []string{"homelab"},
	})
	if err != nil {
		t.Fatalf("failed creating token: %v", err)
	}

	mcpClient, err := client.NewStreamableHttpClient(
		ts.URL+"/mcp",
		transport.WithStreamableHTTPHost("mem.internal"),
		transport.WithHTTPHeaders(map[string]string{
			"Authorization": "Bearer " + agentTok.Token,
		}),
	)
	if err != nil {
		t.Fatalf("failed creating streamable client: %v", err)
	}
	defer mcpClient.Close()

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "client", Version: "1.0"}
	initRes, err := mcpClient.Initialize(ctx, initReq)
	if err != nil {
		t.Fatalf("Streamable Initialize failed: %v", err)
	}
	if initRes.ServerInfo.Name != "agent-memory" {
		t.Fatalf("unexpected server info name: %s", initRes.ServerInfo.Name)
	}

	toolsRes, err := mcpClient.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("Streamable ListTools failed: %v", err)
	}
	if len(toolsRes.Tools) == 0 {
		t.Fatalf("expected tools from streamable client, got 0")
	}
}

func TestHostHeaderMemInternal(t *testing.T) {
	handler, tokenStore, _, _ := setupTestServer(t)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	agentTok, err := tokenStore.CreateToken(ctx, AgentToken{
		AgentID:      "test_agent",
		ReadBuckets:  []string{"homelab"},
		WriteBuckets: []string{"homelab"},
	})
	if err != nil {
		t.Fatalf("failed creating token: %v", err)
	}

	mcpClient, err := client.NewSSEMCPClient(
		ts.URL+"/sse?token="+agentTok.Token,
		transport.WithHTTPHost("mem.internal"),
	)
	if err != nil {
		t.Fatalf("failed creating client: %v", err)
	}
	defer mcpClient.Close()

	if err := mcpClient.Start(ctx); err != nil {
		t.Fatalf("Start failed with mem.internal Host: %v", err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "client", Version: "1.0"}
	initRes, err := mcpClient.Initialize(ctx, initReq)
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	if initRes.ServerInfo.Name != "agent-memory" {
		t.Fatalf("unexpected server info name: %s", initRes.ServerInfo.Name)
	}

	toolsRes, err := mcpClient.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools failed with mem.internal Host: %v", err)
	}
	if len(toolsRes.Tools) == 0 {
		t.Fatalf("expected tools from mem.internal Host, got 0")
	}
}


