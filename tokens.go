package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrTokenNotFound = errors.New("token not found")
	ErrTokenRevoked  = errors.New("token is revoked")
	ErrTokenExists   = errors.New("token already exists")
)

type AgentToken struct {
	Token        string    `json:"token"`
	AgentID      string    `json:"agent_id"`
	Description  string    `json:"description,omitempty"`
	ReadBuckets  []string  `json:"read_buckets"`
	WriteBuckets []string  `json:"write_buckets"`
	Revoked      bool      `json:"revoked"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type AgentTokenUpdate struct {
	AgentID      *string   `json:"agent_id,omitempty"`
	Description  *string   `json:"description,omitempty"`
	ReadBuckets  *[]string `json:"read_buckets,omitempty"`
	WriteBuckets *[]string `json:"write_buckets,omitempty"`
	Revoked      *bool     `json:"revoked,omitempty"`
}

type TokenStore interface {
	CreateToken(ctx context.Context, token AgentToken) (*AgentToken, error)
	GetToken(ctx context.Context, token string) (*AgentToken, error)
	ListTokens(ctx context.Context, agentID string, includeRevoked bool) ([]AgentToken, error)
	UpdateToken(ctx context.Context, token string, update AgentTokenUpdate) (*AgentToken, error)
	RevokeToken(ctx context.Context, token string) error
	DeleteToken(ctx context.Context, token string) error
}

func GenerateSecureToken(prefix string) (string, error) {
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	if prefix == "" {
		prefix = "mem_"
	}
	return prefix + hex.EncodeToString(bytes), nil
}

// DBTokenStore implements TokenStore backed by PostgreSQL
type DBTokenStore struct {
	db *pgxpool.Pool
}

func NewDBTokenStore(db *pgxpool.Pool) *DBTokenStore {
	return &DBTokenStore{db: db}
}

func (s *DBTokenStore) CreateToken(ctx context.Context, t AgentToken) (*AgentToken, error) {
	if strings.TrimSpace(t.Token) == "" {
		generated, err := GenerateSecureToken("mem_")
		if err != nil {
			return nil, fmt.Errorf("failed generating token: %w", err)
		}
		t.Token = generated
	}
	if strings.TrimSpace(t.AgentID) == "" {
		return nil, fmt.Errorf("agent_id is required")
	}
	if t.ReadBuckets == nil {
		t.ReadBuckets = []string{}
	}
	if t.WriteBuckets == nil {
		t.WriteBuckets = []string{}
	}

	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now

	_, err := s.db.Exec(ctx, `
		INSERT INTO agent_tokens (token, agent_id, description, read_buckets, write_buckets, revoked, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		t.Token, t.AgentID, t.Description, t.ReadBuckets, t.WriteBuckets, t.Revoked, t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
			return nil, ErrTokenExists
		}
		return nil, err
	}

	return &t, nil
}

func (s *DBTokenStore) GetToken(ctx context.Context, token string) (*AgentToken, error) {
	var t AgentToken
	err := s.db.QueryRow(ctx, `
		SELECT token, agent_id, description, read_buckets, write_buckets, revoked, created_at, updated_at
		FROM agent_tokens
		WHERE token = $1`, token,
	).Scan(&t.Token, &t.AgentID, &t.Description, &t.ReadBuckets, &t.WriteBuckets, &t.Revoked, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTokenNotFound
		}
		return nil, err
	}
	return &t, nil
}

func (s *DBTokenStore) ListTokens(ctx context.Context, agentID string, includeRevoked bool) ([]AgentToken, error) {
	query := `
		SELECT token, agent_id, description, read_buckets, write_buckets, revoked, created_at, updated_at
		FROM agent_tokens
		WHERE (1=1)`
	args := []any{}
	argIdx := 1

	if agentID != "" {
		query += fmt.Sprintf(" AND agent_id = $%d", argIdx)
		args = append(args, agentID)
		argIdx++
	}
	if !includeRevoked {
		query += fmt.Sprintf(" AND revoked = false")
	}
	query += " ORDER BY created_at DESC"

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []AgentToken
	for rows.Next() {
		var t AgentToken
		if err := rows.Scan(&t.Token, &t.AgentID, &t.Description, &t.ReadBuckets, &t.WriteBuckets, &t.Revoked, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

func (s *DBTokenStore) UpdateToken(ctx context.Context, token string, update AgentTokenUpdate) (*AgentToken, error) {
	existing, err := s.GetToken(ctx, token)
	if err != nil {
		return nil, err
	}

	if update.AgentID != nil {
		existing.AgentID = *update.AgentID
	}
	if update.Description != nil {
		existing.Description = *update.Description
	}
	if update.ReadBuckets != nil {
		existing.ReadBuckets = *update.ReadBuckets
	}
	if update.WriteBuckets != nil {
		existing.WriteBuckets = *update.WriteBuckets
	}
	if update.Revoked != nil {
		existing.Revoked = *update.Revoked
	}
	existing.UpdatedAt = time.Now().UTC()

	tag, err := s.db.Exec(ctx, `
		UPDATE agent_tokens
		SET agent_id = $1, description = $2, read_buckets = $3, write_buckets = $4, revoked = $5, updated_at = $6
		WHERE token = $7`,
		existing.AgentID, existing.Description, existing.ReadBuckets, existing.WriteBuckets, existing.Revoked, existing.UpdatedAt, token,
	)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrTokenNotFound
	}

	return existing, nil
}

func (s *DBTokenStore) RevokeToken(ctx context.Context, token string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE agent_tokens
		SET revoked = true, updated_at = now()
		WHERE token = $1`, token,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrTokenNotFound
	}
	return nil
}

func (s *DBTokenStore) DeleteToken(ctx context.Context, token string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM agent_tokens WHERE token = $1`, token)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrTokenNotFound
	}
	return nil
}

// --- In-Memory Token Store for Tests and Local Execution ---

type InMemoryTokenStore struct {
	mu     sync.RWMutex
	tokens map[string]*AgentToken
}

func NewInMemoryTokenStore() *InMemoryTokenStore {
	return &InMemoryTokenStore{
		tokens: make(map[string]*AgentToken),
	}
}

func (m *InMemoryTokenStore) CreateToken(ctx context.Context, t AgentToken) (*AgentToken, error) {
	if strings.TrimSpace(t.Token) == "" {
		generated, err := GenerateSecureToken("mem_")
		if err != nil {
			return nil, err
		}
		t.Token = generated
	}
	if strings.TrimSpace(t.AgentID) == "" {
		return nil, fmt.Errorf("agent_id is required")
	}
	if t.ReadBuckets == nil {
		t.ReadBuckets = []string{}
	}
	if t.WriteBuckets == nil {
		t.WriteBuckets = []string{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.tokens[t.Token]; exists {
		return nil, ErrTokenExists
	}

	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now

	cp := t
	m.tokens[t.Token] = &cp
	return &cp, nil
}

func (m *InMemoryTokenStore) GetToken(ctx context.Context, token string) (*AgentToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	t, exists := m.tokens[token]
	if !exists {
		return nil, ErrTokenNotFound
	}
	cp := *t
	return &cp, nil
}

func (m *InMemoryTokenStore) ListTokens(ctx context.Context, agentID string, includeRevoked bool) ([]AgentToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var list []AgentToken
	for _, t := range m.tokens {
		if agentID != "" && t.AgentID != agentID {
			continue
		}
		if !includeRevoked && t.Revoked {
			continue
		}
		list = append(list, *t)
	}
	return list, nil
}

func (m *InMemoryTokenStore) UpdateToken(ctx context.Context, token string, update AgentTokenUpdate) (*AgentToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, exists := m.tokens[token]
	if !exists {
		return nil, ErrTokenNotFound
	}

	if update.AgentID != nil {
		t.AgentID = *update.AgentID
	}
	if update.Description != nil {
		t.Description = *update.Description
	}
	if update.ReadBuckets != nil {
		t.ReadBuckets = *update.ReadBuckets
	}
	if update.WriteBuckets != nil {
		t.WriteBuckets = *update.WriteBuckets
	}
	if update.Revoked != nil {
		t.Revoked = *update.Revoked
	}
	t.UpdatedAt = time.Now().UTC()

	cp := *t
	return &cp, nil
}

func (m *InMemoryTokenStore) RevokeToken(ctx context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, exists := m.tokens[token]
	if !exists {
		return ErrTokenNotFound
	}
	t.Revoked = true
	t.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *InMemoryTokenStore) DeleteToken(ctx context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.tokens[token]; !exists {
		return ErrTokenNotFound
	}
	delete(m.tokens, token)
	return nil
}
