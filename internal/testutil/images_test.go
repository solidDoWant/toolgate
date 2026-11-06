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
		"s3":         {testutil.S3Image, "ghcr.io/versity/versitygw:v1.10.0@sha256:89cad13cc981986ee284cbafea4613f9533ce6fd5f37a2c96c4c4fb8adb3a316"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, test.prefix, test.image)
		})
	}
}
