CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS agent_memories (
    id BIGSERIAL PRIMARY KEY,
    bucket TEXT NOT NULL,                        -- e.g., 'homelab', 'webshop', 'agent_alice'
    tier TEXT NOT NULL DEFAULT 'semantic',       -- 'core' (always pinned) or 'semantic' (searched)
    subject TEXT NOT NULL,                       -- Entity or topic key
    content TEXT NOT NULL,                       -- The distilled fact
    metadata JSONB NOT NULL DEFAULT '{}',        -- Source, confidence, S3 URI link
    embedding halfvec(768),                      -- 16-bit float vector
    fts tsvector GENERATED ALWAYS AS (to_tsvector('simple', subject || ' ' || content)) STORED,
    superseded_by BIGINT REFERENCES agent_memories(id),
    valid_from TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_until TIMESTAMPTZ                      -- NULL = currently active fact
);

-- Disable TOAST compression on the vector column to save Pi CPU
ALTER TABLE agent_memories ALTER COLUMN embedding SET STORAGE PLAIN;

-- 1. Partial B-Tree index for fast lookup of active rows per bucket
CREATE INDEX IF NOT EXISTS idx_memories_active_bucket 
ON agent_memories (bucket, tier) 
WHERE valid_until IS NULL;

-- 2. GIN index for exact keyword matching (SKUs, hostnames, IPs, names)
CREATE INDEX IF NOT EXISTS idx_memories_fts 
ON agent_memories USING gin (fts) 
WHERE valid_until IS NULL;

-- 3. Partial HNSW index (ONLY indexes currently valid memories!)
CREATE INDEX IF NOT EXISTS idx_memories_hnsw 
ON agent_memories USING hnsw (embedding halfvec_cosine_ops) 
WITH (m = 16, ef_construction = 64)
WHERE valid_until IS NULL;

-- Dynamic agent authentication tokens table
CREATE TABLE IF NOT EXISTS agent_tokens (
    token TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    read_buckets TEXT[] NOT NULL DEFAULT '{}',
    write_buckets TEXT[] NOT NULL DEFAULT '{}',
    revoked BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_agent_tokens_agent_id ON agent_tokens(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_tokens_revoked ON agent_tokens(revoked);