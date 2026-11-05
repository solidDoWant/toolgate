package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/config"
)

func loadResolvers(t *testing.T, resolvers string) (config.Config, error) {
	t.Helper()

	path := writeFile(t, "databaseURL: "+validDatabaseURL+"\noidcIssuerURL: https://idp.example\noidcAudience: toolgate\nresolvers:\n"+resolvers)

	return config.Load(path, envFrom(nil))
}

func TestResolversSetting(t *testing.T) {
	tests := []struct {
		name        string
		resolvers   string
		wantTimeout time.Duration
		wantProblem string
	}{
		{
			name:        "valid with the default timeout",
			resolvers:   "  - {name: github-app, url: \"https://resolver.example/resolve\", audience: aud, caFile: /ca.pem}\n",
			wantTimeout: 10 * time.Second,
		},
		{
			name:        "timeout of exactly 60s",
			resolvers:   "  - {name: test, url: \"https://resolver.example\", audience: aud, timeout: 60s}\n",
			wantTimeout: 60 * time.Second,
		},
		{
			name:        "timeout over 60s",
			resolvers:   "  - {name: test, url: \"https://resolver.example\", audience: aud, timeout: 60001ms}\n",
			wantProblem: "resolvers[0].timeout: must be at most 60s",
		},
		{
			name:        "http url",
			resolvers:   "  - {name: test, url: \"http://resolver.example\", audience: aud}\n",
			wantProblem: "resolvers[0].url",
		},
		{
			name:        "bad name",
			resolvers:   "  - {name: Test, url: \"https://resolver.example\", audience: aud}\n",
			wantProblem: "resolvers[0].name",
		},
		{
			name:        "duplicate name",
			resolvers:   "  - {name: test, url: \"https://a.example\", audience: a}\n  - {name: test, url: \"https://b.example\", audience: b}\n",
			wantProblem: "resolvers[1].name: \"test\" is listed twice",
		},
		{
			name:        "missing audience",
			resolvers:   "  - {name: test, url: \"https://resolver.example\"}\n",
			wantProblem: "resolvers[0].audience: required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := loadResolvers(t, test.resolvers)
			if test.wantProblem != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantProblem)

				return
			}

			require.NoError(t, err)
			require.Len(t, cfg.Resolvers, 1)
			assert.Equal(t, test.wantTimeout, cfg.Resolvers[0].ResolverTimeout())
		})
	}
}
