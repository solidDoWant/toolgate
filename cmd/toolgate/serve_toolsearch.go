package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/embeddings"
)

// maxEmbeddingsKeyBytes bounds the embeddings API key file.
const maxEmbeddingsKeyBytes = 4 << 10

// newToolSearchEmbedder returns the embeddings client of tool search, or nil
// when no endpoint is configured. It registers the client's metrics.
func newToolSearchEmbedder(cfg config.Config, metrics *embeddings.Metrics) (*embeddings.Client, error) {
	if cfg.ToolSearchEmbeddingsURL == "" {
		return nil, nil //nolint:nilnil // Nil means embeddings are off.
	}

	var apiKey string

	if cfg.ToolSearchEmbeddingsAPIKeyFile != "" {
		key, err := readSmallFile(cfg.ToolSearchEmbeddingsAPIKeyFile, maxEmbeddingsKeyBytes)
		if err != nil {
			return nil, fmt.Errorf("toolSearchEmbeddingsAPIKeyFile: %w", err)
		}

		apiKey = strings.TrimSpace(string(key))
	}

	client, err := embeddings.New(embeddings.Options{
		URL: cfg.ToolSearchEmbeddingsURL, Model: cfg.ToolSearchEmbeddingsModel,
		APIKey: apiKey, Timeout: time.Duration(cfg.ToolSearchEmbeddingsTimeout),
	}, metrics)
	if err != nil {
		return nil, fmt.Errorf("tool search embeddings: %w", err)
	}

	return client, nil
}

// newToolSearchReranker returns the reranker of tool search, or nil when no
// endpoint is configured.
func newToolSearchReranker(cfg config.Config, metrics *embeddings.Metrics) (*embeddings.Reranker, error) {
	if cfg.ToolSearchRerankURL == "" {
		return nil, nil //nolint:nilnil // Nil means reranking is off.
	}

	var apiKey string

	if cfg.ToolSearchRerankAPIKeyFile != "" {
		key, err := readSmallFile(cfg.ToolSearchRerankAPIKeyFile, maxEmbeddingsKeyBytes)
		if err != nil {
			return nil, fmt.Errorf("toolSearchRerankAPIKeyFile: %w", err)
		}

		apiKey = strings.TrimSpace(string(key))
	}

	reranker, err := embeddings.NewReranker(embeddings.Options{
		URL: cfg.ToolSearchRerankURL, Model: cfg.ToolSearchRerankModel,
		APIKey: apiKey, Timeout: time.Duration(cfg.ToolSearchRerankTimeout),
	}, metrics)
	if err != nil {
		return nil, fmt.Errorf("tool search rerank: %w", err)
	}

	return reranker, nil
}
