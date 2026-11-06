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
		"postgres": {testutil.PostgresImage, "mirror.gcr.io/library/postgres:17-alpine@sha256:266d3c257594e195b309ae9d1805e54c788f1d7b375f56bf6c367aac6c917e7b"},
		"s3":       {testutil.S3Image, "mirror.gcr.io/versity/versitygw:v1.10.0@sha256:89cad13cc981986ee284cbafea4613f9533ce6fd5f37a2c96c4c4fb8adb3a316"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, test.prefix, test.image)
		})
	}
}
