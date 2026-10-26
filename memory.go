package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

type MemoryRecord struct {
	ID       int64          `json:"id"`
	Bucket   string         `json:"bucket"`
	Tier     string         `json:"tier,omitempty"`
	Subject  string         `json:"subject"`
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Score    float64        `json:"score,omitempty"`
}

type MemoryStore interface {
	HydratePromptContext(ctx context.Context, allowedBuckets []string, userMessage string, topK int) (core []MemoryRecord, recalled []MemoryRecord, err error)
	HybridSearch(ctx context.Context, allowedBuckets []string, query string, limit int) ([]MemoryRecord, error)
	RememberFact(ctx context.Context, writableBuckets []string, targetBucket, tier, subject, content string, metadata map[string]any, supersedesID *int64) (int64, error)
	InvalidateFact(ctx context.Context, writableBuckets []string, id int64) error
	GetCoreMemories(ctx context.Context, allowedBuckets []string) ([]MemoryRecord, error)
	FormatPromptContext(core []MemoryRecord, recalled []MemoryRecord) string
}

// Store is the production PostgreSQL + pgvector backed memory store.
type Store struct {
	db       *pgxpool.Pool
	embedder Embedder
}

func NewStore(db *pgxpool.Pool, embedder Embedder) *Store {
	return &Store{
		db:       db,
		embedder: embedder,
	}
}

// FormatPromptContext formats core and recalled memories into a clean markdown block.
func (s *Store) FormatPromptContext(core []MemoryRecord, recalled []MemoryRecord) string {
	return formatPromptContext(core, recalled)
}

func formatPromptContext(core []MemoryRecord, recalled []MemoryRecord) string {
	var b strings.Builder
	if len(core) > 0 {
		b.WriteString("### Core Memory Facts\n")
		for _, c := range core {
			b.WriteString(fmt.Sprintf("- **[%s] %s**: %s\n", c.Bucket, c.Subject, c.Content))
		}
	}
	if len(recalled) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("### Recalled Semantic Memories\n")
		for _, r := range recalled {
			b.WriteString(fmt.Sprintf("- **[%s] %s**: %s\n", r.Bucket, r.Subject, r.Content))
		}
	}
	return b.String()
}

// 1. PASSIVE PRE-INFERENCE HOOK: Call this when preparing the prompt for a turn.
// Returns all pinned 'core' facts + top-K hybrid matches for the user's message,
// strictly restricted to the agent's allowedBuckets.
func (s *Store) HydratePromptContext(ctx context.Context, allowedBuckets []string, userMessage string, topK int) (core []MemoryRecord, recalled []MemoryRecord, err error) {
	if len(allowedBuckets) == 0 {
		return nil, nil, nil
	}

	// A. Fetch pinned Core memories for allowed buckets (0 embedding cost, <1ms)
	rows, err := s.db.Query(ctx, `
		SELECT id, bucket, tier, subject, content, metadata
		FROM agent_memories
		WHERE valid_until IS NULL
		  AND tier = 'core'
		  AND bucket = ANY($1)
		ORDER BY id ASC`, allowedBuckets)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var r MemoryRecord
		var metaJSON []byte
		if err := rows.Scan(&r.ID, &r.Bucket, &r.Tier, &r.Subject, &r.Content, &metaJSON); err != nil {
			return nil, nil, err
		}
		if len(metaJSON) > 0 {
			_ = json.Unmarshal(metaJSON, &r.Metadata)
		}
		core = append(core, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// B. Run Hybrid Search (Vector + Keyword RRF) for relevant semantic memories if query is non-empty
	if strings.TrimSpace(userMessage) != "" && topK > 0 {
		recalled, err = s.HybridSearch(ctx, allowedBuckets, userMessage, topK)
	}

	return core, recalled, err
}

// 2. HYBRID SEARCH (Used by both Passive Hook AND Active Agent 'search_memory' Tool)
// Combines pgvector HNSW cosine distance + Postgres Full-Text Search via Reciprocal Rank Fusion (RRF)
func (s *Store) HybridSearch(ctx context.Context, allowedBuckets []string, query string, limit int) ([]MemoryRecord, error) {
	if len(allowedBuckets) == 0 || strings.TrimSpace(query) == "" || limit <= 0 {
		return []MemoryRecord{}, nil
	}

	vec, err := s.embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	halfVec := pgvector.NewHalfVector(vec)

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Enable pgvector 0.8+ iterative scan so bucket filtering never starves HNSW results
	if _, err := tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = relaxed_order`); err != nil {
		return nil, err
	}

	const rrfSQL = `
	WITH vec_matches AS (
		SELECT id, ROW_NUMBER() OVER (ORDER BY embedding <=> $1) AS rank_ix
		FROM agent_memories
		WHERE valid_until IS NULL AND tier = 'semantic' AND bucket = ANY($2)
		ORDER BY embedding <=> $1
		LIMIT $4 * 2
	),
	fts_matches AS (
		SELECT id, ROW_NUMBER() OVER (ORDER BY ts_rank_cd(fts, websearch_to_tsquery('simple', $3)) DESC) AS rank_ix
		FROM agent_memories
		WHERE valid_until IS NULL AND tier = 'semantic' AND bucket = ANY($2)
		  AND fts @@ websearch_to_tsquery('simple', $3)
		LIMIT $4 * 2
	)
	SELECT m.id, m.bucket, m.tier, m.subject, m.content, m.metadata,
	       COALESCE(1.0 / (60 + v.rank_ix), 0.0) + COALESCE(1.0 / (60 + f.rank_ix), 0.0) AS rrf_score
	FROM vec_matches v
	FULL OUTER JOIN fts_matches f ON v.id = f.id
	JOIN agent_memories m ON m.id = COALESCE(v.id, f.id)
	ORDER BY rrf_score DESC
	LIMIT $4;`

	rows, err := tx.Query(ctx, rrfSQL, halfVec, allowedBuckets, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []MemoryRecord
	for rows.Next() {
		var r MemoryRecord
		var metaJSON []byte
		if err := rows.Scan(&r.ID, &r.Bucket, &r.Tier, &r.Subject, &r.Content, &metaJSON, &r.Score); err != nil {
			return nil, err
		}
		if len(metaJSON) > 0 {
			_ = json.Unmarshal(metaJSON, &r.Metadata)
		}
		results = append(results, r)
	}
	return results, tx.Commit(ctx)
}

// 3. ACTIVE WRITE / UPSERT TOOL: Agent calls this to store a new fact or supersede an outdated ID.
func (s *Store) RememberFact(
	ctx context.Context,
	writableBuckets []string,
	targetBucket, tier, subject, content string,
	metadata map[string]any,
	supersedesID *int64,
) (int64, error) {
	if !slices.Contains(writableBuckets, targetBucket) {
		return 0, fmt.Errorf("permission denied: agent cannot write to bucket %q", targetBucket)
	}

	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier == "" {
		tier = "semantic"
	}
	if tier != "core" && tier != "semantic" {
		return 0, fmt.Errorf("invalid tier %q: must be 'core' or 'semantic'", tier)
	}

	if metadata == nil {
		metadata = make(map[string]any)
	}
	metaBytes, err := json.Marshal(metadata)
	if err != nil {
		return 0, fmt.Errorf("invalid metadata: %w", err)
	}

	vec, err := s.embedder.Embed(ctx, subject+": "+content)
	if err != nil {
		return 0, err
	}
	halfVec := pgvector.NewHalfVector(vec)

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var newID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO agent_memories (bucket, tier, subject, content, metadata, embedding)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`,
		targetBucket, tier, subject, content, metaBytes, halfVec,
	).Scan(&newID)
	if err != nil {
		return 0, err
	}

	// Atomically invalidate the old memory if the agent specified supersedes_id
	if supersedesID != nil && *supersedesID > 0 {
		_, err = tx.Exec(ctx, `
			UPDATE agent_memories
			SET valid_until = now(), superseded_by = $1
			WHERE id = $2 AND bucket = ANY($3) AND valid_until IS NULL`,
			newID, *supersedesID, writableBuckets,
		)
		if err != nil {
			return 0, err
		}
	}

	return newID, tx.Commit(ctx)
}

// InvalidateFact marks a memory record as superseded/inactive.
func (s *Store) InvalidateFact(ctx context.Context, writableBuckets []string, id int64) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE agent_memories
		SET valid_until = now()
		WHERE id = $1 AND bucket = ANY($2) AND valid_until IS NULL`,
		id, writableBuckets,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("memory %d not found or not writable", id)
	}
	return nil
}

// GetCoreMemories retrieves all active pinned core facts for the allowed buckets.
func (s *Store) GetCoreMemories(ctx context.Context, allowedBuckets []string) ([]MemoryRecord, error) {
	if len(allowedBuckets) == 0 {
		return []MemoryRecord{}, nil
	}

	rows, err := s.db.Query(ctx, `
		SELECT id, bucket, tier, subject, content, metadata
		FROM agent_memories
		WHERE valid_until IS NULL
		  AND tier = 'core'
		  AND bucket = ANY($1)
		ORDER BY id ASC`, allowedBuckets)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []MemoryRecord
	for rows.Next() {
		var r MemoryRecord
		var metaJSON []byte
		if err := rows.Scan(&r.ID, &r.Bucket, &r.Tier, &r.Subject, &r.Content, &metaJSON); err != nil {
			return nil, err
		}
		if len(metaJSON) > 0 {
			_ = json.Unmarshal(metaJSON, &r.Metadata)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// --- In-Memory Store Implementation for Tests & Local Evaluation ---

type memoryEntry struct {
	record    MemoryRecord
	embedding []float32
	valid     bool
}

type InMemoryStore struct {
	mu       sync.RWMutex
	memories map[int64]*memoryEntry
	nextID   int64
	embedder Embedder
}

func NewInMemoryStore(embedder Embedder) *InMemoryStore {
	if embedder == nil {
		embedder = NewMockEmbedder(768)
	}
	return &InMemoryStore{
		memories: make(map[int64]*memoryEntry),
		embedder: embedder,
	}
}

func (m *InMemoryStore) FormatPromptContext(core []MemoryRecord, recalled []MemoryRecord) string {
	return formatPromptContext(core, recalled)
}

func (m *InMemoryStore) HydratePromptContext(ctx context.Context, allowedBuckets []string, userMessage string, topK int) ([]MemoryRecord, []MemoryRecord, error) {
	if len(allowedBuckets) == 0 {
		return nil, nil, nil
	}
	core, err := m.GetCoreMemories(ctx, allowedBuckets)
	if err != nil {
		return nil, nil, err
	}
	var recalled []MemoryRecord
	if strings.TrimSpace(userMessage) != "" && topK > 0 {
		recalled, err = m.HybridSearch(ctx, allowedBuckets, userMessage, topK)
		if err != nil {
			return nil, nil, err
		}
	}
	return core, recalled, nil
}

func (m *InMemoryStore) HybridSearch(ctx context.Context, allowedBuckets []string, query string, limit int) ([]MemoryRecord, error) {
	if len(allowedBuckets) == 0 || strings.TrimSpace(query) == "" || limit <= 0 {
		return []MemoryRecord{}, nil
	}

	queryVec, err := m.embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	type scoredItem struct {
		rec   MemoryRecord
		score float64
	}
	var scored []scoredItem

	queryLower := strings.ToLower(query)
	for _, entry := range m.memories {
		if !entry.valid || entry.record.Tier != "semantic" {
			continue
		}
		if !slices.Contains(allowedBuckets, entry.record.Bucket) {
			continue
		}

		// Cosine similarity
		var dot float64
		for i := 0; i < len(queryVec) && i < len(entry.embedding); i++ {
			dot += float64(queryVec[i] * entry.embedding[i])
		}

		// Basic keyword match bonus
		var textScore float64
		subjLower := strings.ToLower(entry.record.Subject)
		contLower := strings.ToLower(entry.record.Content)
		if strings.Contains(subjLower, queryLower) || strings.Contains(contLower, queryLower) {
			textScore = 0.5
		}

		totalScore := dot + textScore
		rec := entry.record
		rec.Score = totalScore
		scored = append(scored, scoredItem{rec: rec, score: totalScore})
	}

	slices.SortFunc(scored, func(a, b scoredItem) int {
		if b.score > a.score {
			return 1
		} else if b.score < a.score {
			return -1
		}
		return 0
	})

	if len(scored) > limit {
		scored = scored[:limit]
	}

	res := make([]MemoryRecord, len(scored))
	for i, s := range scored {
		res[i] = s.rec
	}
	return res, nil
}

func (m *InMemoryStore) RememberFact(
	ctx context.Context,
	writableBuckets []string,
	targetBucket, tier, subject, content string,
	metadata map[string]any,
	supersedesID *int64,
) (int64, error) {
	if !slices.Contains(writableBuckets, targetBucket) {
		return 0, fmt.Errorf("permission denied: agent cannot write to bucket %q", targetBucket)
	}

	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier == "" {
		tier = "semantic"
	}
	if tier != "core" && tier != "semantic" {
		return 0, fmt.Errorf("invalid tier %q: must be 'core' or 'semantic'", tier)
	}

	vec, err := m.embedder.Embed(ctx, subject+": "+content)
	if err != nil {
		return 0, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	newID := atomic.AddInt64(&m.nextID, 1)
	if metadata == nil {
		metadata = make(map[string]any)
	}

	m.memories[newID] = &memoryEntry{
		record: MemoryRecord{
			ID:       newID,
			Bucket:   targetBucket,
			Tier:     tier,
			Subject:  subject,
			Content:  content,
			Metadata: metadata,
		},
		embedding: vec,
		valid:     true,
	}

	if supersedesID != nil && *supersedesID > 0 {
		if old, exists := m.memories[*supersedesID]; exists {
			if slices.Contains(writableBuckets, old.record.Bucket) {
				old.valid = false
			}
		}
	}

	return newID, nil
}

func (m *InMemoryStore) InvalidateFact(ctx context.Context, writableBuckets []string, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.memories[id]
	if !ok || !entry.valid {
		return fmt.Errorf("memory %d not found or already invalid", id)
	}
	if !slices.Contains(writableBuckets, entry.record.Bucket) {
		return fmt.Errorf("permission denied to invalidate memory %d in bucket %q", id, entry.record.Bucket)
	}
	entry.valid = false
	return nil
}

func (m *InMemoryStore) GetCoreMemories(ctx context.Context, allowedBuckets []string) ([]MemoryRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var results []MemoryRecord
	for _, entry := range m.memories {
		if entry.valid && entry.record.Tier == "core" && slices.Contains(allowedBuckets, entry.record.Bucket) {
			results = append(results, entry.record)
		}
	}
	slices.SortFunc(results, func(a, b MemoryRecord) int {
		return int(a.ID - b.ID)
	})
	return results, nil
}
