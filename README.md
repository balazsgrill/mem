# Agent Memory Management Layer (`mem`)

A lightweight, resident memory management layer for AI agents powered by PostgreSQL and **pgvector**.

It provides:
1. **Prompt-Time Extending Operation**: Pre-inference hook to dynamically hydrate LLM prompts with pinned core memories and hybrid-recalled semantic memories restricted to the agent's allowed buckets.
2. **Resident SSE MCP Server**: Server-Sent Events (SSE) interface serving multiple concurrent agents in parallel with dynamic authentication.
3. **Dynamic Authentication & Multi-Tenancy**: Dynamic access tokens stored in the database, defining granular read and write bucket permissions, managed via a Master Token-authenticated HTTP JSON API.
4. **Hybrid Search (RRF)**: Combines pgvector cosine distance (with `halfvec` and pgvector 0.8+ iterative scan) and PostgreSQL Full-Text Search using Reciprocal Rank Fusion.

---

## Architecture Overview

```
                      ┌──────────────────────────────────────────────┐
                      │             Master / Admin HTTP API          │
                      │       (Authorization: Bearer <MASTER_TOKEN>) │
                      └──────────────────────┬───────────────────────┘
                                             │
                        ┌────────────────────▼────────────────────┐
                        │      Token Management (/api/tokens)     │
                        │      Create / Read / Update / Revoke    │
                        └────────────────────┬────────────────────┘
                                             │
                                             ▼
                 ┌────────────────────────────────────────────────────────┐
                 │             agent_tokens (PostgreSQL)                  │
                 │  token | agent_id | read_buckets | write_buckets       │
                 └───────────────────────────┬────────────────────────────┘
                                             │ Dynamic Auth
        ┌────────────────────────────────────┼────────────────────────────────────┐
        │                                    │                                    │
┌───────▼────────────────────────┐           │                     ┌──────────────▼─────────────────┐
│ Prompt-Time Hydration Endpoint │           │                     │   Resident SSE MCP Server      │
│     POST /api/prompt/hydrate   │           │                     │      GET /sse, POST /message   │
│     (Pre-inference context)    │           │                     │  (Parallel Agent Tool Access)  │
└───────┬────────────────────────┘           │                     └──────────────┬─────────────────┘
        │                                    │                                    │
        │                                    ▼                                    │
        │                        ┌───────────────────────┐                        │
        └───────────────────────►│  agent_memories (DB)  │◄───────────────────────┘
                                 │  HNSW + FTS RRF Search│
                                 └───────────────────────┘
```

---

## Features

### 1. Granular Bucket Permissions
Each agent token has two sets of buckets:
- `read_buckets`: Allowed buckets for semantic search and core memory hydration.
- `write_buckets`: Allowed buckets for remembering and invalidating facts.

An agent cannot read or modify any memory outside of its assigned buckets. Multiple agents can connect in parallel without data leakage.

### 2. Prompt-Time Extending Hook
Before running LLM inference, orchestrators call `POST /api/prompt/hydrate`. It:
- Retrieves active pinned **core** memories for the agent's buckets (<1ms, no embedding cost).
- Performs hybrid semantic search for the user's message using RRF.
- Returns structured JSON records and ready-to-inject Markdown (`prompt_context`).

### 3. Dynamic Authentication
- Token access records are stored in PostgreSQL (`agent_tokens` table).
- Dynamic changes (creation, bucket expansion, revocation) take effect immediately without restarting the server.
- Protected by a configurable `MASTER_TOKEN`.

### 4. High-Performance Hybrid Search
- Uses pgvector `halfvec(768)` (16-bit floating point vectors) to minimize memory usage and cache footprints.
- Executes `SET LOCAL hnsw.iterative_scan = relaxed_order` to prevent HNSW index starvation when filtering by bucket.
- Fuses vector cosine similarity with Postgres Full-Text Search (`tsvector`) via Reciprocal Rank Fusion (RRF).

---

## Database Schema

Run `schema.sql` against your PostgreSQL database:

```sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS agent_memories (
    id BIGSERIAL PRIMARY KEY,
    bucket TEXT NOT NULL,
    tier TEXT NOT NULL DEFAULT 'semantic',       -- 'core' (pinned) or 'semantic' (searched)
    subject TEXT NOT NULL,
    content TEXT NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}',
    embedding halfvec(768),
    fts tsvector GENERATED ALWAYS AS (to_tsvector('simple', subject || ' ' || content)) STORED,
    superseded_by BIGINT REFERENCES agent_memories(id),
    valid_from TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_until TIMESTAMPTZ                      -- NULL = currently active fact
);

ALTER TABLE agent_memories ALTER COLUMN embedding SET STORAGE PLAIN;

CREATE INDEX IF NOT EXISTS idx_memories_active_bucket 
ON agent_memories (bucket, tier) 
WHERE valid_until IS NULL;

CREATE INDEX IF NOT EXISTS idx_memories_fts 
ON agent_memories USING gin (fts) 
WHERE valid_until IS NULL;

CREATE INDEX IF NOT EXISTS idx_memories_hnsw 
ON agent_memories USING hnsw (embedding halfvec_cosine_ops) 
WITH (m = 16, ef_construction = 64)
WHERE valid_until IS NULL;

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
```

---

## Configuration

The application can be configured via environment variables or CLI flags:

| Environment Variable | CLI Flag | Default | Description |
|----------------------|----------|---------|-------------|
| `DATABASE_URL` | `-db` | *(empty = in-memory)* | PostgreSQL connection URL |
| `LISTEN_ADDR` | `-addr` | `:8080` | Address for HTTP and SSE server |
| `MASTER_TOKEN` | `-master-token` | *(auto-generated)* | Admin bearer token for Token API |
| `BASE_URL` | `-base-url` | *(empty)* | Optional server base URL for reverse proxies |
| `AUTO_MIGRATE` | `-auto-migrate` | `true` | Automatically run schema migrations on startup |
| `EMBEDDER_TYPE` | `-embedder` | `mock` / auto | Embedder type: `openai`, `ollama`, or `mock` |
| `OPENAI_API_KEY` | `-openai-key` | *(empty)* | OpenAI API key |
| `OPENAI_BASE_URL` | `-openai-base-url` | `https://api.openai.com/v1` | OpenAI compatible endpoint |
| `OPENAI_MODEL` | `-openai-model` | `text-embedding-3-small` | OpenAI embedding model |
| `OLLAMA_BASE_URL` | `-ollama-base-url` | `http://localhost:11434` | Ollama endpoint |
| `OLLAMA_MODEL` | `-ollama-model` | `nomic-embed-text` | Ollama model name |

---

---

## Web UI Control Plane

The server embeds a rich, progressive **Angular** frontend directly inside the compiled binary.

### Accessing the UI
Access the control plane using your configured master token:
```
http://localhost:8080/ui/<MASTER_TOKEN>
# or
http://localhost:8080/ui?token=<MASTER_TOKEN>
```
If you visit `http://localhost:8080/ui/` directly, an authentication modal prompts for your master token. The session is preserved via cookie and localStorage.

### UI Features
1. **Buckets & Memory Management**:
   - Directory of all buckets with live stats (active, core, semantic counts).
   - Empty bucket action (soft invalidation or permanent wipe).
   - Filter memories by search term, tier (Core vs Semantic), and superseded status.
   - Add new memory facts with interactive metadata JSON editor.
   - Edit existing memory facts (automatically recalculates vector embeddings).
   - Invalidate or permanently delete individual memory facts.
2. **Agent Token Management**:
   - List active and revoked tokens with quick copy-to-clipboard.
   - Create new tokens with custom or auto-generated token IDs.
   - Add/remove read and write bucket permissions using interactive tags/chips.
   - Toggle token revocation or delete permanently.
3. **Prompt Hydration Sandbox**:
   - Interactive tester to preview the exact system prompt context that an LLM receives at runtime.

---

## HTTP REST API

All administrative endpoints can be authenticated with `Authorization: Bearer <MASTER_TOKEN>`, `X-Master-Token: <MASTER_TOKEN>`, `?token=<MASTER_TOKEN>`, or the `mem_master_token` cookie.

### 1. Bucket Management API

- **List Buckets**: `GET /api/buckets`
  Returns array of bucket summaries with counts:
  ```json
  [
    {
      "bucket": "homelab",
      "active_count": 14,
      "core_count": 3,
      "semantic_count": 11,
      "total_count": 16
    }
  ]
  ```

- **Empty Bucket**: `DELETE /api/buckets/{bucket}?permanent=false`
  Invalidates (or permanently deletes if `permanent=true`) all active memories in the specified bucket.

### 2. Memory Entry Management API

- **List Entries**: `GET /api/entries?bucket=homelab&tier=core&q=DNS&include_superseded=false&limit=50&offset=0`
  (Also accessible via `GET /api/buckets/{bucket}/entries`).
  Returns `{ "entries": [...], "total": 1 }`.

- **Get Entry by ID**: `GET /api/entries/{id}`

- **Add Entry**: `POST /api/entries` (or `POST /api/buckets/{bucket}/entries`)
  ```json
  {
    "bucket": "homelab",
    "tier": "semantic",
    "subject": "Storage",
    "content": "NFS storage pool mounted at /mnt/data",
    "metadata": { "source": "admin" }
  }
  ```

- **Update Entry**: `PUT /api/entries/{id}`
  ```json
  {
    "subject": "Storage Updated",
    "content": "NFS storage pool mounted at /mnt/storage",
    "tier": "core"
  }
  ```

- **Delete Entry**: `DELETE /api/entries/{id}?permanent=false`
  Invalidates memory (or permanently deletes if `permanent=true`).

---

### 3. Token Management API
All token management endpoints require: `Authorization: Bearer <MASTER_TOKEN>` or `X-Master-Token: <MASTER_TOKEN>`.

#### Create an Agent Token
`POST /api/tokens`
```bash
curl -X POST http://localhost:8080/api/tokens \
  -H "Authorization: Bearer <MASTER_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "agent_id": "alice",
    "description": "Agent Alice personal assistant",
    "read_buckets": ["homelab", "alice_private"],
    "write_buckets": ["alice_private"]
  }'
```
Response:
```json
{
  "token": "mem_d6c544fa9ec9cb266e76cf0a2b0ec034f4bc807e3240e340",
  "agent_id": "alice",
  "description": "Agent Alice personal assistant",
  "read_buckets": ["homelab", "alice_private"],
  "write_buckets": ["alice_private"],
  "revoked": false,
  "created_at": "2026-10-26T08:00:00Z",
  "updated_at": "2026-10-26T08:00:00Z"
}
```

#### List Tokens
`GET /api/tokens?agent_id=alice&include_revoked=false`

#### Get Token Details
`GET /api/tokens/{token}`

#### Update Token
`PUT /api/tokens/{token}`
```json
{
  "description": "Updated description",
  "read_buckets": ["homelab", "alice_private", "shared"],
  "write_buckets": ["alice_private"]
}
```

#### Revoke or Delete Token
- Revoke: `DELETE /api/tokens/{token}` (sets `revoked = true`)
- Delete: `DELETE /api/tokens/{token}?permanent=true` (removes row)

---

### 4. Prompt Hydration API (Pre-Inference Extension)
`POST /api/prompt/hydrate` (or `/api/hydrate`)

Can be authenticated with:
- An **Agent Token**: `Authorization: Bearer <AGENT_TOKEN>` (automatically scopes to the agent's `read_buckets`).
- The **Master Token**: `Authorization: Bearer <MASTER_TOKEN>` (can specify any bucket).

Request:
```bash
curl -X POST http://localhost:8080/api/prompt/hydrate \
  -H "Authorization: Bearer mem_d6c544fa9ec9..." \
  -H "Content-Type: application/json" \
  -d '{
    "query": "What is the IP of the DNS server and how is storage mounted?",
    "top_k": 5
  }'
```

Response:
```json
{
  "core": [
    {
      "id": 1,
      "bucket": "homelab",
      "tier": "core",
      "subject": "DNS",
      "content": "Pi-hole DNS is running at 192.168.1.5"
    }
  ],
  "recalled": [
    {
      "id": 14,
      "bucket": "homelab",
      "tier": "semantic",
      "subject": "Storage",
      "content": "NFS storage pool mounted at /mnt/data",
      "score": 0.0322
    }
  ],
  "prompt_context": "### Core Memory Facts\n- **[homelab] DNS**: Pi-hole DNS is running at 192.168.1.5\n\n### Recalled Semantic Memories\n- **[homelab] Storage**: NFS storage pool mounted at /mnt/data\n"
}
```

---

## Resident MCP Server (SSE & Streamable HTTP)

The server acts as a resident MCP server implementing the Model Context Protocol supporting both **Streamable HTTP** (MCP 2025+) and **Server-Sent Events** (SSE, legacy MCP).

### 1. Connecting via Streamable HTTP (Recommended for modern MCP clients)
Modern clients (e.g. Cursor, Claude, Antigravity) connect directly to `/mcp`:
```http
POST /mcp
Authorization: Bearer <AGENT_TOKEN>
Content-Type: application/json
```
*(Query parameter `?token=<AGENT_TOKEN>` is also supported).*

### 2. Connecting via SSE
Agents establish an SSE stream with their bearer token:
```
GET /sse
Authorization: Bearer <AGENT_TOKEN>
```
*(Query parameter `GET /sse?token=<AGENT_TOKEN>` is also supported for clients unable to send custom headers during EventSource handshake).*

The server responds with an SSE event indicating the session message endpoint:
```http
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive

event: endpoint
data: /message?sessionId=4a66fbc6-98ec-40f4-8a47-f5dc817b3855&token=mem_d6c...
```

Subsequent JSON-RPC 2.0 messages are sent via `POST /message?sessionId=...` and results are streamed back through the SSE connection.

---

### Exposed MCP Tools

#### 1. `search_memory`
Performs hybrid vector + full-text search across allowed read buckets.
- `query` *(string, required)*: Search query.
- `limit` *(number, optional)*: Max memories (default 5, max 50).
- `bucket` *(string, optional)*: Specific bucket (must be within `read_buckets`).

#### 2. `remember_fact`
Stores a new fact or updates an existing fact.
- `bucket` *(string, required)*: Target bucket (must be in `write_buckets`).
- `subject` *(string, required)*: Topic or entity key.
- `content` *(string, required)*: The distilled fact.
- `tier` *(string, optional)*: `'core'` (always pinned in prompt context) or `'semantic'` (searchable, default).
- `supersedes_id` *(number, optional)*: Memory ID this new fact replaces.

#### 3. `get_core_memories`
Fetches all active pinned core facts for the agent's read buckets.
- `bucket` *(string, optional)*: Optional bucket filter.

#### 4. `forget_fact`
Invalidates an active memory fact by ID.
- `id` *(number, required)*: The memory ID to invalidate (must belong to a writable bucket).

---

## Build and Run

### Run Tests
```bash
go test -v -race .
```

### Build Binary
```bash
go build -o mem .
```

### Run Locally (In-Memory Development Mode)
```bash
./mem -addr :8080
```

### Run with PostgreSQL + pgvector
```bash
export DATABASE_URL="postgres://postgres:password@localhost:5432/mem?sslmode=disable"
export MASTER_TOKEN="super-secret-admin-token"
export OPENAI_API_KEY="sk-..."
./mem
```
