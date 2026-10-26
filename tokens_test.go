package main

import (
	"context"
	"errors"
	"testing"
)

func TestInMemoryTokenStore(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryTokenStore()

	// 1. Create Token
	created, err := store.CreateToken(ctx, AgentToken{
		AgentID:      "agent_alice",
		Description:  "Alice agent token",
		ReadBuckets:  []string{"homelab", "alice_private"},
		WriteBuckets: []string{"alice_private"},
	})
	if err != nil {
		t.Fatalf("failed creating token: %v", err)
	}
	if created.Token == "" {
		t.Fatalf("expected non-empty generated token")
	}
	if created.AgentID != "agent_alice" {
		t.Fatalf("expected agent_id agent_alice, got %s", created.AgentID)
	}

	// 2. Get Token
	found, err := store.GetToken(ctx, created.Token)
	if err != nil {
		t.Fatalf("failed retrieving token: %v", err)
	}
	if found.AgentID != "agent_alice" {
		t.Fatalf("expected agent_alice, got %s", found.AgentID)
	}

	// 3. List Tokens
	tokens, err := store.ListTokens(ctx, "agent_alice", false)
	if err != nil {
		t.Fatalf("failed listing tokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(tokens))
	}

	// 4. Update Token
	newDesc := "Updated Alice description"
	newRead := []string{"homelab", "alice_private", "shared"}
	updated, err := store.UpdateToken(ctx, created.Token, AgentTokenUpdate{
		Description: &newDesc,
		ReadBuckets: &newRead,
	})
	if err != nil {
		t.Fatalf("failed updating token: %v", err)
	}
	if updated.Description != newDesc {
		t.Fatalf("expected updated description %q, got %q", newDesc, updated.Description)
	}
	if len(updated.ReadBuckets) != 3 {
		t.Fatalf("expected 3 read buckets, got %d", len(updated.ReadBuckets))
	}

	// 5. Revoke Token
	if err := store.RevokeToken(ctx, created.Token); err != nil {
		t.Fatalf("failed revoking token: %v", err)
	}
	afterRevoke, err := store.GetToken(ctx, created.Token)
	if err != nil {
		t.Fatalf("failed getting revoked token: %v", err)
	}
	if !afterRevoke.Revoked {
		t.Fatalf("expected revoked=true")
	}

	// Active list shouldn't include revoked
	activeTokens, err := store.ListTokens(ctx, "", false)
	if err != nil {
		t.Fatalf("failed listing active tokens: %v", err)
	}
	if len(activeTokens) != 0 {
		t.Fatalf("expected 0 active tokens, got %d", len(activeTokens))
	}

	// 6. Delete Token
	if err := store.DeleteToken(ctx, created.Token); err != nil {
		t.Fatalf("failed deleting token: %v", err)
	}
	_, err = store.GetToken(ctx, created.Token)
	if !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("expected ErrTokenNotFound, got %v", err)
	}
}
