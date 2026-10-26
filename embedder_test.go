package main

import (
	"context"
	"math"
	"testing"
)

func cosineSimilarity(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i] * b[i])
		normA += float64(a[i] * a[i])
		normB += float64(b[i] * b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func TestMockEmbedder(t *testing.T) {
	emb := NewMockEmbedder(768)
	if emb.Dimension() != 768 {
		t.Fatalf("expected dimension 768, got %d", emb.Dimension())
	}

	ctx := context.Background()

	// 1. Check unit normalization
	vec1, err := emb.Embed(ctx, "raspberry pi homelab server cluster")
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}
	if len(vec1) != 768 {
		t.Fatalf("expected 768 floats, got %d", len(vec1))
	}

	var sumSq float64
	for _, v := range vec1 {
		sumSq += float64(v * v)
	}
	if math.Abs(sumSq-1.0) > 1e-4 {
		t.Fatalf("expected unit vector length, sum of squares was %f", sumSq)
	}

	// 2. Determinism
	vec2, err := emb.Embed(ctx, "raspberry pi homelab server cluster")
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}
	simSelf := cosineSimilarity(vec1, vec2)
	if math.Abs(simSelf-1.0) > 1e-5 {
		t.Fatalf("expected self-similarity 1.0, got %f", simSelf)
	}

	// 3. Semantic similarity
	vecRelated, err := emb.Embed(ctx, "homelab cluster server setup")
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}
	vecUnrelated, err := emb.Embed(ctx, "chocolate chip pancake baking recipe")
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}

	simRelated := cosineSimilarity(vec1, vecRelated)
	simUnrelated := cosineSimilarity(vec1, vecUnrelated)

	if simRelated <= simUnrelated {
		t.Fatalf("expected related text to have higher similarity than unrelated text (related: %f, unrelated: %f)",
			simRelated, simUnrelated)
	}
}
