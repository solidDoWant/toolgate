package main

import (
	"context"
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
func newToolSearchEmbedder(cfg config.Config, metrics *embeddings.Metrics, onDemand func(context.Context), scaledDown func(context.Context) bool) (*embeddings.Client, error) {
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
		OnDemand: onDemand, ScaledDown: scaledDown,
	}, metrics)
	if err != nil {
		return nil, fmt.Errorf("tool search embeddings: %w", err)
	}

	return client, nil
}
