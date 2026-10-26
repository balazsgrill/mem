package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dimension() int
}

// MockEmbedder provides deterministic, unit-normalized vectors without external API calls.
// Useful for unit testing, offline use, and development environments.
type MockEmbedder struct {
	dim int
}

func NewMockEmbedder(dim int) *MockEmbedder {
	if dim <= 0 {
		dim = 768
	}
	return &MockEmbedder{dim: dim}
}

func (m *MockEmbedder) Dimension() int {
	return m.dim
}

func (m *MockEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vec := make([]float32, m.dim)
	words := strings.Fields(strings.ToLower(text))
	if len(words) == 0 {
		return vec, nil
	}

	for _, word := range words {
		h := fnv.New64a()
		h.Write([]byte(word))
		hashVal := h.Sum64()

		// Distribute energy across dimension indices based on hash
		for i := 0; i < m.dim; i++ {
			var buf [8]byte
			binary.LittleEndian.PutUint64(buf[:], hashVal^uint64(i*31))
			h2 := fnv.New32a()
			h2.Write(buf[:])
			val := float32(h2.Sum32()) / float32(math.MaxUint32)
			vec[i] += (val - 0.5)
		}
	}

	// Normalize vector to unit length
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	norm := math.Sqrt(sumSq)
	if norm > 0 {
		for i := range vec {
			vec[i] = float32(float64(vec[i]) / norm)
		}
	}

	return vec, nil
}

// OpenAIEmbedder connects to OpenAI-compatible /v1/embeddings endpoints (OpenAI, FastChat, vLLM, etc.)
type OpenAIEmbedder struct {
	baseURL    string
	apiKey     string
	model      string
	dimensions int
	client     *http.Client
}

func NewOpenAIEmbedder(baseURL, apiKey, model string, dimensions int) *OpenAIEmbedder {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "text-embedding-3-small"
	}
	if dimensions <= 0 {
		dimensions = 768
	}
	return &OpenAIEmbedder{
		baseURL:    baseURL,
		apiKey:     apiKey,
		model:      model,
		dimensions: dimensions,
		client:     &http.Client{Timeout: 30 * time.Second},
	}
}

func (o *OpenAIEmbedder) Dimension() int {
	return o.dimensions
}

type openAIEmbedRequest struct {
	Input      string `json:"input"`
	Model      string `json:"model"`
	Dimensions int    `json:"dimensions,omitempty"`
}

type openAIEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (o *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	reqBody := openAIEmbedRequest{
		Input: text,
		Model: o.model,
	}
	// text-embedding-3 supports dimensions param
	if strings.Contains(o.model, "embedding-3") {
		reqBody.Dimensions = o.dimensions
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/embeddings", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai embed request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read embed response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai embed API error (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var parsed openAIEmbedResponse
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return nil, fmt.Errorf("failed to decode embed response: %w", err)
	}

	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("openai embed error: %s", parsed.Error.Message)
	}

	if len(parsed.Data) == 0 || len(parsed.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding returned from API")
	}

	return parsed.Data[0].Embedding, nil
}

// OllamaEmbedder connects to local Ollama instance
type OllamaEmbedder struct {
	baseURL string
	model   string
	dim     int
	client  *http.Client
}

func NewOllamaEmbedder(baseURL, model string, dim int) *OllamaEmbedder {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	if dim <= 0 {
		dim = 768
	}
	return &OllamaEmbedder{
		baseURL: baseURL,
		model:   model,
		dim:     dim,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (o *OllamaEmbedder) Dimension() int {
	return o.dim
}

type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type ollamaEmbedResponse struct {
	Embedding []float32 `json:"embedding"`
	Error     string    `json:"error,omitempty"`
}

func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	reqBody := ollamaEmbedRequest{
		Model:  o.model,
		Prompt: text,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/embeddings", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read ollama response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama embed error (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var parsed ollamaEmbedResponse
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return nil, fmt.Errorf("failed to decode ollama response: %w", err)
	}

	if parsed.Error != "" {
		return nil, fmt.Errorf("ollama embed error: %s", parsed.Error)
	}

	if len(parsed.Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding returned from ollama")
	}

	return parsed.Embedding, nil
}
