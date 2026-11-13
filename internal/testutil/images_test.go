package testutil_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestTestImagesComeFromMirror(t *testing.T) {
	tests := map[string]struct {
		image  string
		prefix string
	}{
		"postgres":   {testutil.PostgresImage, "ghcr.io/cloudnative-pg/postgresql:17-standard-trixie@sha256:4d0aac315c4d8c3202252bd06fea83b16951cc57234a7b610eeaa382ff4482d0"},
		"embeddings": {testutil.EmbeddingsImage, "ghcr.io/huggingface/text-embeddings-inference:cpu-1.9@sha256:2538ea1c9640d3763b15af668039d24172d063b42337b0c27796fc2be180c78d"},
		"s3":         {testutil.S3Image, "ghcr.io/versity/versitygw:v1.11.0@sha256:8319a530d08d537426e2f855bae5280709795dc73a3f0c347973ef915d6343c5"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, test.prefix, test.image)
		})
	}
}
