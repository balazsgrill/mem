package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

var (
	ErrMemoryNotFound = errors.New("memory entry not found")
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

type MemoryDetail struct {
	ID           int64          `json:"id"`
	Bucket       string         `json:"bucket"`
	Tier         string         `json:"tier"`
	Subject      string         `json:"subject"`
	Content      string         `json:"content"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	SupersededBy *int64         `json:"superseded_by,omitempty"`
	ValidFrom    time.Time      `json:"valid_from"`
	ValidUntil   *time.Time     `json:"valid_until,omitempty"`
}

type BucketSummary struct {
	Bucket        string `json:"bucket"`
	ActiveCount   int64  `json:"active_count"`
	CoreCount     int64  `json:"core_count"`
	SemanticCount int64  `json:"semantic_count"`
	TotalCount    int64  `json:"total_count"`
}

type MemoryStore interface {
	HydratePromptContext(ctx context.Context, allowedBuckets []string, userMessage string, topK int) (core []MemoryRecord, recalled []MemoryRecord, err error)
	HybridSearch(ctx context.Context, allowedBuckets []string, query string, limit int) ([]MemoryRecord, error)
	RememberFact(ctx context.Context, writableBuckets []string, targetBucket, tier, subject, content string, metadata map[string]any, supersedesID *int64) (int64, error)
	InvalidateFact(ctx context.Context, writableBuckets []string, id int64) error
	GetCoreMemories(ctx context.Context, allowedBuckets []string) ([]MemoryRecord, error)
	FormatPromptContext(core []MemoryRecord, recalled []MemoryRecord) string

	// Master / UI Management Operations
	ListBuckets(ctx context.Context) ([]BucketSummary, error)
	EmptyBucket(ctx context.Context, bucket string, permanent bool) (int64, error)
	ListEntries(ctx context.Context, bucket string, tier string, query string, includeSuperseded bool, limit, offset int) ([]MemoryDetail, int64, error)
	GetEntry(ctx context.Context, id int64) (*MemoryDetail, error)
	AddEntry(ctx context.Context, bucket, tier, subject, content string, metadata map[string]any) (*MemoryDetail, error)
	UpdateEntry(ctx context.Context, id int64, bucket, tier, subject, content string, metadata map[string]any) (*MemoryDetail, error)
	DeleteEntry(ctx context.Context, id int64, permanent bool) error
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

// 1. PASSIVE PRE-INFERENCE HOOK
func (s *Store) HydratePromptContext(ctx context.Context, allowedBuckets []string, userMessage string, topK int) (core []MemoryRecord, recalled []MemoryRecord, err error) {
	if len(allowedBuckets) == 0 {
		return nil, nil, nil
	}

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

	if strings.TrimSpace(userMessage) != "" && topK > 0 {
		recalled, err = s.HybridSearch(ctx, allowedBuckets, userMessage, topK)
	}

	return core, recalled, err
}

// 2. HYBRID SEARCH
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

// 3. ACTIVE WRITE / UPSERT TOOL
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

// Master / UI Management Operations for Store

func (s *Store) ListBuckets(ctx context.Context) ([]BucketSummary, error) {
	rows, err := s.db.Query(ctx, `
		SELECT 
			bucket,
			COUNT(*) FILTER (WHERE valid_until IS NULL) AS active_count,
			COUNT(*) FILTER (WHERE valid_until IS NULL AND tier = 'core') AS core_count,
			COUNT(*) FILTER (WHERE valid_until IS NULL AND tier = 'semantic') AS semantic_count,
			COUNT(*) AS total_count
		FROM agent_memories
		GROUP BY bucket
		ORDER BY bucket ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []BucketSummary
	for rows.Next() {
		var bs BucketSummary
		if err := rows.Scan(&bs.Bucket, &bs.ActiveCount, &bs.CoreCount, &bs.SemanticCount, &bs.TotalCount); err != nil {
			return nil, err
		}
		summaries = append(summaries, bs)
	}
	return summaries, rows.Err()
}

func (s *Store) EmptyBucket(ctx context.Context, bucket string, permanent bool) (int64, error) {
	if permanent {
		res, err := s.db.Exec(ctx, `DELETE FROM agent_memories WHERE bucket = $1`, bucket)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected(), nil
	}

	res, err := s.db.Exec(ctx, `
		UPDATE agent_memories
		SET valid_until = now()
		WHERE bucket = $1 AND valid_until IS NULL`, bucket)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

func (s *Store) ListEntries(ctx context.Context, bucket string, tier string, query string, includeSuperseded bool, limit, offset int) ([]MemoryDetail, int64, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	var conditions []string
	var args []any
	argIdx := 1

	if bucket != "" {
		conditions = append(conditions, fmt.Sprintf("bucket = $%d", argIdx))
		args = append(args, bucket)
		argIdx++
	}

	if tier != "" {
		conditions = append(conditions, fmt.Sprintf("tier = $%d", argIdx))
		args = append(args, tier)
		argIdx++
	}

	if !includeSuperseded {
		conditions = append(conditions, "valid_until IS NULL")
	}

	if strings.TrimSpace(query) != "" {
		conditions = append(conditions, fmt.Sprintf("(subject ILIKE $%d OR content ILIKE $%d)", argIdx, argIdx))
		args = append(args, "%"+query+"%")
		argIdx++
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	// Count total
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM agent_memories %s", whereClause)
	var total int64
	if err := s.db.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Fetch page
	dataQuery := fmt.Sprintf(`
		SELECT id, bucket, tier, subject, content, metadata, superseded_by, valid_from, valid_until
		FROM agent_memories
		%s
		ORDER BY id DESC
		LIMIT $%d OFFSET $%d`, whereClause, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := s.db.Query(ctx, dataQuery, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var entries []MemoryDetail
	for rows.Next() {
		var d MemoryDetail
		var metaJSON []byte
		if err := rows.Scan(&d.ID, &d.Bucket, &d.Tier, &d.Subject, &d.Content, &metaJSON, &d.SupersededBy, &d.ValidFrom, &d.ValidUntil); err != nil {
			return nil, 0, err
		}
		if len(metaJSON) > 0 {
			_ = json.Unmarshal(metaJSON, &d.Metadata)
		}
		entries = append(entries, d)
	}

	return entries, total, rows.Err()
}

func (s *Store) GetEntry(ctx context.Context, id int64) (*MemoryDetail, error) {
	var d MemoryDetail
	var metaJSON []byte
	err := s.db.QueryRow(ctx, `
		SELECT id, bucket, tier, subject, content, metadata, superseded_by, valid_from, valid_until
		FROM agent_memories
		WHERE id = $1`, id,
	).Scan(&d.ID, &d.Bucket, &d.Tier, &d.Subject, &d.Content, &metaJSON, &d.SupersededBy, &d.ValidFrom, &d.ValidUntil)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrMemoryNotFound
		}
		return nil, err
	}
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &d.Metadata)
	}
	return &d, nil
}

func (s *Store) AddEntry(ctx context.Context, bucket, tier, subject, content string, metadata map[string]any) (*MemoryDetail, error) {
	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier == "" {
		tier = "semantic"
	}
	if tier != "core" && tier != "semantic" {
		return nil, fmt.Errorf("invalid tier %q: must be 'core' or 'semantic'", tier)
	}
	if strings.TrimSpace(bucket) == "" {
		return nil, fmt.Errorf("bucket is required")
	}
	if strings.TrimSpace(subject) == "" {
		return nil, fmt.Errorf("subject is required")
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("content is required")
	}

	if metadata == nil {
		metadata = make(map[string]any)
	}
	metaBytes, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("invalid metadata: %w", err)
	}

	vec, err := s.embedder.Embed(ctx, subject+": "+content)
	if err != nil {
		return nil, err
	}
	halfVec := pgvector.NewHalfVector(vec)

	var d MemoryDetail
	var metaJSON []byte
	err = s.db.QueryRow(ctx, `
		INSERT INTO agent_memories (bucket, tier, subject, content, metadata, embedding)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, bucket, tier, subject, content, metadata, superseded_by, valid_from, valid_until`,
		bucket, tier, subject, content, metaBytes, halfVec,
	).Scan(&d.ID, &d.Bucket, &d.Tier, &d.Subject, &d.Content, &metaJSON, &d.SupersededBy, &d.ValidFrom, &d.ValidUntil)
	if err != nil {
		return nil, err
	}
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &d.Metadata)
	}
	return &d, nil
}

func (s *Store) UpdateEntry(ctx context.Context, id int64, bucket, tier, subject, content string, metadata map[string]any) (*MemoryDetail, error) {
	existing, err := s.GetEntry(ctx, id)
	if err != nil {
		return nil, err
	}

	if bucket != "" {
		existing.Bucket = bucket
	}
	if tier != "" {
		tier = strings.ToLower(strings.TrimSpace(tier))
		if tier != "core" && tier != "semantic" {
			return nil, fmt.Errorf("invalid tier %q: must be 'core' or 'semantic'", tier)
		}
		existing.Tier = tier
	}
	if subject != "" {
		existing.Subject = subject
	}
	if content != "" {
		existing.Content = content
	}
	if metadata != nil {
		existing.Metadata = metadata
	}

	metaBytes, err := json.Marshal(existing.Metadata)
	if err != nil {
		return nil, fmt.Errorf("invalid metadata: %w", err)
	}

	vec, err := s.embedder.Embed(ctx, existing.Subject+": "+existing.Content)
	if err != nil {
		return nil, err
	}
	halfVec := pgvector.NewHalfVector(vec)

	var d MemoryDetail
	var metaJSON []byte
	err = s.db.QueryRow(ctx, `
		UPDATE agent_memories
		SET bucket = $1, tier = $2, subject = $3, content = $4, metadata = $5, embedding = $6
		WHERE id = $7
		RETURNING id, bucket, tier, subject, content, metadata, superseded_by, valid_from, valid_until`,
		existing.Bucket, existing.Tier, existing.Subject, existing.Content, metaBytes, halfVec, id,
	).Scan(&d.ID, &d.Bucket, &d.Tier, &d.Subject, &d.Content, &metaJSON, &d.SupersededBy, &d.ValidFrom, &d.ValidUntil)
	if err != nil {
		return nil, err
	}
	if len(metaJSON) > 0 {
		_ = json.Unmarshal(metaJSON, &d.Metadata)
	}
	return &d, nil
}

func (s *Store) DeleteEntry(ctx context.Context, id int64, permanent bool) error {
	if permanent {
		tag, err := s.db.Exec(ctx, `DELETE FROM agent_memories WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrMemoryNotFound
		}
		return nil
	}

	tag, err := s.db.Exec(ctx, `
		UPDATE agent_memories
		SET valid_until = now()
		WHERE id = $1 AND valid_until IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrMemoryNotFound
	}
	return nil
}

// --- In-Memory Store Implementation for Tests & Local Evaluation ---

type memoryEntry struct {
	record       MemoryRecord
	embedding    []float32
	valid        bool
	supersededBy *int64
	validFrom    time.Time
	validUntil   *time.Time
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

		var dot float64
		for i := 0; i < len(queryVec) && i < len(entry.embedding); i++ {
			dot += float64(queryVec[i] * entry.embedding[i])
		}

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

	now := time.Now().UTC()
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
		validFrom: now,
	}

	if supersedesID != nil && *supersedesID > 0 {
		if old, exists := m.memories[*supersedesID]; exists {
			if slices.Contains(writableBuckets, old.record.Bucket) {
				old.valid = false
				old.supersededBy = &newID
				old.validUntil = &now
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
	now := time.Now().UTC()
	entry.valid = false
	entry.validUntil = &now
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

// Master / UI Management Operations for InMemoryStore

func (m *InMemoryStore) ListBuckets(ctx context.Context) ([]BucketSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	bucketMap := make(map[string]*BucketSummary)
	for _, entry := range m.memories {
		b := entry.record.Bucket
		bs, exists := bucketMap[b]
		if !exists {
			bs = &BucketSummary{Bucket: b}
			bucketMap[b] = bs
		}
		bs.TotalCount++
		if entry.valid {
			bs.ActiveCount++
			if entry.record.Tier == "core" {
				bs.CoreCount++
			} else if entry.record.Tier == "semantic" {
				bs.SemanticCount++
			}
		}
	}

	summaries := make([]BucketSummary, 0, len(bucketMap))
	for _, bs := range bucketMap {
		summaries = append(summaries, *bs)
	}
	slices.SortFunc(summaries, func(a, b BucketSummary) int {
		return strings.Compare(a.Bucket, b.Bucket)
	})
	return summaries, nil
}

func (m *InMemoryStore) EmptyBucket(ctx context.Context, bucket string, permanent bool) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var count int64
	now := time.Now().UTC()
	for id, entry := range m.memories {
		if entry.record.Bucket == bucket {
			if permanent {
				delete(m.memories, id)
				count++
			} else if entry.valid {
				entry.valid = false
				entry.validUntil = &now
				count++
			}
		}
	}
	return count, nil
}

func (m *InMemoryStore) ListEntries(ctx context.Context, bucket string, tier string, query string, includeSuperseded bool, limit, offset int) ([]MemoryDetail, int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var filtered []MemoryDetail
	queryLower := strings.ToLower(query)

	for _, entry := range m.memories {
		if !includeSuperseded && !entry.valid {
			continue
		}
		if bucket != "" && entry.record.Bucket != bucket {
			continue
		}
		if tier != "" && entry.record.Tier != tier {
			continue
		}
		if query != "" {
			sub := strings.ToLower(entry.record.Subject)
			cnt := strings.ToLower(entry.record.Content)
			if !strings.Contains(sub, queryLower) && !strings.Contains(cnt, queryLower) {
				continue
			}
		}

		filtered = append(filtered, MemoryDetail{
			ID:           entry.record.ID,
			Bucket:       entry.record.Bucket,
			Tier:         entry.record.Tier,
			Subject:      entry.record.Subject,
			Content:      entry.record.Content,
			Metadata:     entry.record.Metadata,
			SupersededBy: entry.supersededBy,
			ValidFrom:    entry.validFrom,
			ValidUntil:   entry.validUntil,
		})
	}

	slices.SortFunc(filtered, func(a, b MemoryDetail) int {
		return int(b.ID - a.ID)
	})

	total := int64(len(filtered))
	if offset >= len(filtered) {
		return []MemoryDetail{}, total, nil
	}
	end := offset + limit
	if end > len(filtered) {
		end = len(filtered)
	}

	return filtered[offset:end], total, nil
}

func (m *InMemoryStore) GetEntry(ctx context.Context, id int64) (*MemoryDetail, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, ok := m.memories[id]
	if !ok {
		return nil, ErrMemoryNotFound
	}
	return &MemoryDetail{
		ID:           entry.record.ID,
		Bucket:       entry.record.Bucket,
		Tier:         entry.record.Tier,
		Subject:      entry.record.Subject,
		Content:      entry.record.Content,
		Metadata:     entry.record.Metadata,
		SupersededBy: entry.supersededBy,
		ValidFrom:    entry.validFrom,
		ValidUntil:   entry.validUntil,
	}, nil
}

func (m *InMemoryStore) AddEntry(ctx context.Context, bucket, tier, subject, content string, metadata map[string]any) (*MemoryDetail, error) {
	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier == "" {
		tier = "semantic"
	}
	if tier != "core" && tier != "semantic" {
		return nil, fmt.Errorf("invalid tier %q: must be 'core' or 'semantic'", tier)
	}
	if strings.TrimSpace(bucket) == "" {
		return nil, fmt.Errorf("bucket is required")
	}
	if strings.TrimSpace(subject) == "" {
		return nil, fmt.Errorf("subject is required")
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("content is required")
	}

	vec, err := m.embedder.Embed(ctx, subject+": "+content)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	newID := atomic.AddInt64(&m.nextID, 1)
	if metadata == nil {
		metadata = make(map[string]any)
	}
	now := time.Now().UTC()

	m.memories[newID] = &memoryEntry{
		record: MemoryRecord{
			ID:       newID,
			Bucket:   bucket,
			Tier:     tier,
			Subject:  subject,
			Content:  content,
			Metadata: metadata,
		},
		embedding: vec,
		valid:     true,
		validFrom: now,
	}

	return &MemoryDetail{
		ID:        newID,
		Bucket:    bucket,
		Tier:      tier,
		Subject:   subject,
		Content:   content,
		Metadata:  metadata,
		ValidFrom: now,
	}, nil
}

func (m *InMemoryStore) UpdateEntry(ctx context.Context, id int64, bucket, tier, subject, content string, metadata map[string]any) (*MemoryDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.memories[id]
	if !ok {
		return nil, ErrMemoryNotFound
	}

	if bucket != "" {
		entry.record.Bucket = bucket
	}
	if tier != "" {
		tier = strings.ToLower(strings.TrimSpace(tier))
		if tier != "core" && tier != "semantic" {
			return nil, fmt.Errorf("invalid tier %q: must be 'core' or 'semantic'", tier)
		}
		entry.record.Tier = tier
	}
	if subject != "" {
		entry.record.Subject = subject
	}
	if content != "" {
		entry.record.Content = content
	}
	if metadata != nil {
		entry.record.Metadata = metadata
	}

	vec, err := m.embedder.Embed(ctx, entry.record.Subject+": "+entry.record.Content)
	if err != nil {
		return nil, err
	}
	entry.embedding = vec

	return &MemoryDetail{
		ID:           entry.record.ID,
		Bucket:       entry.record.Bucket,
		Tier:         entry.record.Tier,
		Subject:      entry.record.Subject,
		Content:      entry.record.Content,
		Metadata:     entry.record.Metadata,
		SupersededBy: entry.supersededBy,
		ValidFrom:    entry.validFrom,
		ValidUntil:   entry.validUntil,
	}, nil
}

func (m *InMemoryStore) DeleteEntry(ctx context.Context, id int64, permanent bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.memories[id]
	if !ok {
		return ErrMemoryNotFound
	}

	if permanent {
		delete(m.memories, id)
		return nil
	}

	now := time.Now().UTC()
	entry.valid = false
	entry.validUntil = &now
	return nil
}
