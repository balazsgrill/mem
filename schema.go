package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EnsureSchema runs the database migration statements to initialize tables and indexes.
func EnsureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	statements := []string{
		`CREATE EXTENSION IF NOT EXISTS vector;`,
		`CREATE TABLE IF NOT EXISTS agent_memories (
			id BIGSERIAL PRIMARY KEY,
			bucket TEXT NOT NULL,
			tier TEXT NOT NULL DEFAULT 'semantic',
			subject TEXT NOT NULL,
			content TEXT NOT NULL,
			metadata JSONB NOT NULL DEFAULT '{}',
			embedding halfvec(768),
			fts tsvector GENERATED ALWAYS AS (to_tsvector('simple', subject || ' ' || content)) STORED,
			superseded_by BIGINT REFERENCES agent_memories(id),
			valid_from TIMESTAMPTZ NOT NULL DEFAULT now(),
			valid_until TIMESTAMPTZ
		);`,
		`ALTER TABLE agent_memories ALTER COLUMN embedding SET STORAGE PLAIN;`,
		`CREATE INDEX IF NOT EXISTS idx_memories_active_bucket 
		ON agent_memories (bucket, tier) 
		WHERE valid_until IS NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_memories_fts 
		ON agent_memories USING gin (fts) 
		WHERE valid_until IS NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_memories_hnsw 
		ON agent_memories USING hnsw (embedding halfvec_cosine_ops) 
		WITH (m = 16, ef_construction = 64)
		WHERE valid_until IS NULL;`,
		`CREATE TABLE IF NOT EXISTS agent_tokens (
			token TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			read_buckets TEXT[] NOT NULL DEFAULT '{}',
			write_buckets TEXT[] NOT NULL DEFAULT '{}',
			revoked BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);`,
		`CREATE INDEX IF NOT EXISTS idx_agent_tokens_agent_id ON agent_tokens(agent_id);`,
		`CREATE INDEX IF NOT EXISTS idx_agent_tokens_revoked ON agent_tokens(revoked);`,
	}

	for _, stmt := range statements {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := pool.Exec(ctx, stmt); err != nil {
			// Some statements like ALTER COLUMN STORAGE PLAIN can error if already altered
			// or extension creation might fail with permission errors if pre-installed.
			if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "storage") {
				continue
			}
			return fmt.Errorf("failed executing schema statement %q: %w", stmt, err)
		}
	}

	return nil
}
