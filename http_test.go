package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func setupTestServer(t *testing.T) (http.Handler, TokenStore, MemoryStore, string) {
	masterToken := "test-master-token-12345"
	tokenStore := NewInMemoryTokenStore()
	embedder := NewMockEmbedder(768)
	memStore := NewInMemoryStore(embedder)
	registry := NewSessionRegistry()

	cfg := ServerConfig{
		MasterToken: masterToken,
		ListenAddr:  ":8080",
	}

	router := BuildRouter(cfg, tokenStore, memStore, registry)
	return router, tokenStore, memStore, masterToken
}

func TestTokenManagementAPI(t *testing.T) {
	router, _, _, masterToken := setupTestServer(t)

	// 1. Unauthenticated request to /api/tokens should return 401
	req, _ := http.NewRequest(http.MethodGet, "/api/tokens", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d", rr.Code)
	}

	// 2. Request with invalid token should return 401
	req, _ = http.NewRequest(http.MethodGet, "/api/tokens", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid master token, got %d", rr.Code)
	}

	// 3. Create Token via POST /api/tokens
	createPayload := map[string]any{
		"agent_id":      "agent_homelab",
		"description":   "Homelab manager agent",
		"read_buckets":  []string{"homelab", "shared"},
		"write_buckets": []string{"homelab"},
	}
	body, _ := json.Marshal(createPayload)
	req, _ = http.NewRequest(http.MethodPost, "/api/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+masterToken)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rr.Code, rr.Body.String())
	}

	var createdToken AgentToken
	if err := json.Unmarshal(rr.Body.Bytes(), &createdToken); err != nil {
		t.Fatalf("failed decoding created token: %v", err)
	}
	if createdToken.Token == "" {
		t.Fatalf("expected non-empty generated token")
	}
	if createdToken.AgentID != "agent_homelab" {
		t.Fatalf("expected agent_id agent_homelab, got %s", createdToken.AgentID)
	}

	// 4. List Tokens via GET /api/tokens
	req, _ = http.NewRequest(http.MethodGet, "/api/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+masterToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rr.Code)
	}
	var tokenList []AgentToken
	if err := json.Unmarshal(rr.Body.Bytes(), &tokenList); err != nil {
		t.Fatalf("failed decoding token list: %v", err)
	}
	if len(tokenList) != 1 {
		t.Fatalf("expected 1 token in list, got %d", len(tokenList))
	}

	// 5. Update Token via PUT /api/tokens/{token}
	updatePayload := map[string]any{
		"description":  "Updated Homelab Agent",
		"read_buckets": []string{"homelab", "shared", "network"},
	}
	updateBody, _ := json.Marshal(updatePayload)
	req, _ = http.NewRequest(http.MethodPut, "/api/tokens/"+createdToken.Token, bytes.NewReader(updateBody))
	req.Header.Set("Authorization", "Bearer "+masterToken)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for update, got %d", rr.Code)
	}

	var updatedToken AgentToken
	_ = json.Unmarshal(rr.Body.Bytes(), &updatedToken)
	if updatedToken.Description != "Updated Homelab Agent" {
		t.Fatalf("expected updated description, got %q", updatedToken.Description)
	}
	if len(updatedToken.ReadBuckets) != 3 {
		t.Fatalf("expected 3 read buckets, got %d", len(updatedToken.ReadBuckets))
	}

	// 6. Revoke Token via DELETE /api/tokens/{token}
	req, _ = http.NewRequest(http.MethodDelete, "/api/tokens/"+createdToken.Token, nil)
	req.Header.Set("Authorization", "Bearer "+masterToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for revoke, got %d", rr.Code)
	}

	// 7. Verify Revoked Token in GET
	req, _ = http.NewRequest(http.MethodGet, "/api/tokens/"+createdToken.Token, nil)
	req.Header.Set("Authorization", "Bearer "+masterToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	var checkRevoked AgentToken
	_ = json.Unmarshal(rr.Body.Bytes(), &checkRevoked)
	if !checkRevoked.Revoked {
		t.Fatalf("expected token to be revoked")
	}
}

func TestPromptHydrationAPI(t *testing.T) {
	router, tokenStore, memStore, masterToken := setupTestServer(t)
	ctx := context.Background()

	// Pre-populate facts in memStore
	// 1. Core fact (always pinned)
	_, err := memStore.RememberFact(ctx, []string{"homelab"}, "homelab", "core", "DNS", "Pi-hole is at 192.168.1.5", nil, nil)
	if err != nil {
		t.Fatalf("failed seeding core fact: %v", err)
	}

	// 2. Semantic facts
	_, err = memStore.RememberFact(ctx, []string{"homelab"}, "homelab", "semantic", "Storage", "NFS mounted on /mnt/storage", nil, nil)
	if err != nil {
		t.Fatalf("failed seeding semantic fact: %v", err)
	}
	_, err = memStore.RememberFact(ctx, []string{"secret"}, "secret", "semantic", "Credentials", "Vault master key is confidential", nil, nil)
	if err != nil {
		t.Fatalf("failed seeding secret fact: %v", err)
	}

	// Create agent token with access ONLY to "homelab"
	agentTok, err := tokenStore.CreateToken(ctx, AgentToken{
		AgentID:      "agent_homelab",
		ReadBuckets:  []string{"homelab"},
		WriteBuckets: []string{"homelab"},
	})
	if err != nil {
		t.Fatalf("failed creating agent token: %v", err)
	}

	// Case A: Call with Agent Token
	hydrateReq := map[string]any{
		"query": "Where is the storage mounted and what is DNS?",
		"top_k": 5,
	}
	body, _ := json.Marshal(hydrateReq)
	req, _ := http.NewRequest(http.MethodPost, "/api/prompt/hydrate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+agentTok.Token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rr.Code, rr.Body.String())
	}

	type HydrateResponse struct {
		Core          []MemoryRecord `json:"core"`
		Recalled      []MemoryRecord `json:"recalled"`
		PromptContext string         `json:"prompt_context"`
	}
	var resp HydrateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed decoding hydrate response: %v", err)
	}

	if len(resp.Core) != 1 {
		t.Fatalf("expected 1 core fact, got %d", len(resp.Core))
	}
	if resp.Core[0].Subject != "DNS" {
		t.Fatalf("expected DNS subject in core facts, got %s", resp.Core[0].Subject)
	}
	if len(resp.Recalled) < 1 {
		t.Fatalf("expected recalled memory for storage")
	}
	if resp.PromptContext == "" {
		t.Fatalf("expected formatted prompt_context string")
	}

	// Case B: Agent tries to request unauthorized bucket "secret" -> 403 Forbidden
	unauthReq := map[string]any{
		"query":   "Vault master key",
		"buckets": []string{"secret"},
		"top_k":   5,
	}
	unauthBody, _ := json.Marshal(unauthReq)
	req, _ = http.NewRequest(http.MethodPost, "/api/prompt/hydrate", bytes.NewReader(unauthBody))
	req.Header.Set("Authorization", "Bearer "+agentTok.Token)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when accessing unauthorized bucket, got %d", rr.Code)
	}

	// Case C: Master Token can query any bucket
	masterReq := map[string]any{
		"query":   "Vault master key",
		"buckets": []string{"secret"},
		"top_k":   5,
	}
	masterBody, _ := json.Marshal(masterReq)
	req, _ = http.NewRequest(http.MethodPost, "/api/prompt/hydrate", bytes.NewReader(masterBody))
	req.Header.Set("Authorization", "Bearer "+masterToken)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for master token query, got %d: %s", rr.Code, rr.Body.String())
	}
	var masterResp HydrateResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &masterResp)
	if len(masterResp.Recalled) == 0 || masterResp.Recalled[0].Subject != "Credentials" {
		t.Fatalf("expected master token to access secret bucket memories")
	}
}

func TestBucketAndEntryManagementAPI(t *testing.T) {
	router, _, _, masterToken := setupTestServer(t)

	// 1. Add entry via POST /api/entries
	addPayload := map[string]any{
		"bucket":   "webshop",
		"tier":     "semantic",
		"subject":  "Catalog",
		"content":  "Product catalog has 1500 items",
		"metadata": map[string]any{"source": "seed"},
	}
	body, _ := json.Marshal(addPayload)
	req, _ := http.NewRequest(http.MethodPost, "/api/entries", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+masterToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rr.Code, rr.Body.String())
	}

	var createdEntry MemoryDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &createdEntry); err != nil {
		t.Fatalf("failed decoding created entry: %v", err)
	}
	if createdEntry.ID == 0 || createdEntry.Subject != "Catalog" {
		t.Fatalf("invalid created entry: %+v", createdEntry)
	}

	// 2. List buckets via GET /api/buckets
	req, _ = http.NewRequest(http.MethodGet, "/api/buckets", nil)
	req.Header.Set("Authorization", "Bearer "+masterToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for list buckets, got %d", rr.Code)
	}
	var buckets []BucketSummary
	_ = json.Unmarshal(rr.Body.Bytes(), &buckets)
	if len(buckets) != 1 || buckets[0].Bucket != "webshop" {
		t.Fatalf("expected webshop bucket in summary, got: %+v", buckets)
	}
	if buckets[0].ActiveCount != 1 {
		t.Fatalf("expected 1 active count, got %d", buckets[0].ActiveCount)
	}

	// 3. List entries via GET /api/buckets/webshop/entries
	req, _ = http.NewRequest(http.MethodGet, "/api/buckets/webshop/entries", nil)
	req.Header.Set("Authorization", "Bearer "+masterToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for list entries, got %d", rr.Code)
	}
	var entryListResp struct {
		Entries []MemoryDetail `json:"entries"`
		Total   int64          `json:"total"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &entryListResp)
	if entryListResp.Total != 1 || len(entryListResp.Entries) != 1 {
		t.Fatalf("expected 1 entry, got total=%d", entryListResp.Total)
	}

	// 4. Update entry via PUT /api/entries/{id}
	updatePayload := map[string]any{
		"subject": "Catalog Updated",
		"content": "Product catalog has 1600 items",
		"tier":    "core",
	}
	updateBody, _ := json.Marshal(updatePayload)
	req, _ = http.NewRequest(http.MethodPut, fmt.Sprintf("/api/entries/%d", createdEntry.ID), bytes.NewReader(updateBody))
	req.Header.Set("Authorization", "Bearer "+masterToken)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for update entry, got %d: %s", rr.Code, rr.Body.String())
	}
	var updatedEntry MemoryDetail
	_ = json.Unmarshal(rr.Body.Bytes(), &updatedEntry)
	if updatedEntry.Subject != "Catalog Updated" || updatedEntry.Tier != "core" {
		t.Fatalf("expected updated subject and core tier, got: %+v", updatedEntry)
	}

	// 5. Delete entry via DELETE /api/entries/{id}
	req, _ = http.NewRequest(http.MethodDelete, fmt.Sprintf("/api/entries/%d", createdEntry.ID), nil)
	req.Header.Set("Authorization", "Bearer "+masterToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for delete entry, got %d", rr.Code)
	}

	// 6. Empty bucket via DELETE /api/buckets/webshop
	req, _ = http.NewRequest(http.MethodDelete, "/api/buckets/webshop?permanent=true", nil)
	req.Header.Set("Authorization", "Bearer "+masterToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for empty bucket, got %d", rr.Code)
	}
}

func TestUIEmbeddedServerAndAuth(t *testing.T) {
	router, _, _, masterToken := setupTestServer(t)

	// 1. Visit /ui/<masterToken> -> redirects to /ui/ and sets cookie
	req, _ := http.NewRequest(http.MethodGet, "/ui/"+masterToken, nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect for /ui/<token>, got %d", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/ui/" {
		t.Fatalf("expected redirect to /ui/, got %s", loc)
	}
	cookies := rr.Result().Cookies()
	var authCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "mem_master_token" {
			authCookie = c
			break
		}
	}
	if authCookie == nil || authCookie.Value != masterToken {
		t.Fatalf("expected mem_master_token cookie set with master token, got: %v", authCookie)
	}

	// 2. Visit /ui?token=<masterToken> -> redirects to /ui/ and sets cookie
	req, _ = http.NewRequest(http.MethodGet, "/ui?token="+masterToken, nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect for /ui?token=..., got %d", rr.Code)
	}

	// 3. Visit /ui/ with cookie -> serves index.html
	req, _ = http.NewRequest(http.MethodGet, "/ui/", nil)
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for /ui/, got %d", rr.Code)
	}
	html := rr.Body.String()
	if !strings.Contains(html, "<app-root>") {
		t.Fatalf("expected embedded index.html with <app-root>, got: %s", html)
	}

	// 4. Visit SPA subroute /ui/buckets -> serves index.html fallback
	req, _ = http.NewRequest(http.MethodGet, "/ui/buckets", nil)
	req.AddCookie(authCookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for /ui/buckets SPA fallback, got %d", rr.Code)
	}

	// 5. Root / redirects to /ui/
	req, _ = http.NewRequest(http.MethodGet, "/", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect for /, got %d", rr.Code)
	}
}

