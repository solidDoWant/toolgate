// Command embeddingsserver stands in for text-embeddings-inference in the e2e
// tests. It serves GET /health and POST /v1/embeddings, and returns a small
// vector that depends on the words of each input. /health answers 503 for the
// first startDelay after the process starts, so a test sees a server that is
// starting before it is ready.
//
// The chart's model download Job runs `uvx`; when this binary runs under that
// name it exits at once, as a finished download.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	dimensions = 8
	startDelay = 10 * time.Second
	maxBody    = 1 << 20
)

func main() {
	if filepath.Base(os.Args[0]) == "uvx" {
		return
	}

	flags := flag.NewFlagSet("embeddingsserver", flag.ExitOnError)
	port := flags.Int("port", 80, "listen port")
	_ = flags.String("model-id", "", "ignored; accepted for the chart's arguments")
	_ = flags.Parse(os.Args[1:])

	started := time.Now()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		if time.Since(started) < startDelay {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}
	})

	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)

			return
		}

		data := make([]map[string]any, len(in.Input))
		for index, text := range in.Input {
			data[index] = map[string]any{"index": index, "embedding": vector(text)}
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	})

	server := &http.Server{Addr: fmt.Sprintf(":%d", *port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "embeddingsserver:", err)

		os.Exit(1)
	}
}

// vector sums a hashed unit vector for each word of text.
func vector(text string) []float32 {
	result := make([]float32, dimensions)

	for _, word := range strings.Fields(strings.ToLower(text)) {
		hash := fnv.New32a()
		_, _ = hash.Write([]byte(word))
		result[hash.Sum32()%dimensions]++
	}

	if text == "" {
		result[0] = 1
	}

	return result
}
