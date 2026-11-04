package version

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGet(t *testing.T) {
	tests := []struct {
		name  string
		built string
		want  string
	}{
		{name: "unstamped build", built: "", want: "dev"},
		{name: "release build", built: "0.1.0", want: "0.1.0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			previous := version
			version = test.built

			t.Cleanup(func() { version = previous })

			assert.Equal(t, test.want, Get())
		})
	}
}
