package toolsearch_test

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type serverFile struct {
	Servers []struct {
		Name   string `json:"name"`
		Source struct {
			Repository string `json:"repository"`
			Licence    string `json:"licence"`
		} `json:"source"`
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	} `json:"servers"`
}

type taskFile struct {
	Tasks []struct {
		ID       string `json:"id"`
		Task     string `json:"task"`
		Expected struct {
			Server string `json:"server"`
			Tool   string `json:"tool"`
		} `json:"expected"`
	} `json:"tasks"`
}

func load(t *testing.T, path string, out any) {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, out))
}

func TestFixtureShape(t *testing.T) {
	var (
		servers serverFile
		tasks   taskFile
	)

	load(t, "servers.json", &servers)
	load(t, "tasks.json", &tasks)

	serverName := regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	toolName := regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

	known := map[string]map[string]bool{}
	total := 0

	for _, server := range servers.Servers {
		assert.Regexp(t, serverName, server.Name)
		assert.NotEmpty(t, server.Source.Repository, server.Name)
		assert.NotEmpty(t, server.Source.Licence, server.Name)
		require.NotContains(t, known, server.Name, "duplicate server")

		known[server.Name] = map[string]bool{}
		for _, tool := range server.Tools {
			total++

			assert.Regexp(t, toolName, tool.Name)
			assert.LessOrEqual(t, len(server.Name+"__"+tool.Name), 64, "prefixed name too long")
			assert.NotEmpty(t, tool.Description, tool.Name)
			assert.Equal(t, "object", tool.InputSchema["type"], tool.Name)
			assert.False(t, known[server.Name][tool.Name], "duplicate tool %s/%s", server.Name, tool.Name)
			known[server.Name][tool.Name] = true
		}
	}

	assert.GreaterOrEqual(t, len(servers.Servers), 8)
	assert.GreaterOrEqual(t, total, 50)

	assert.GreaterOrEqual(t, len(tasks.Tasks), 50)

	ids := map[string]bool{}

	for _, task := range tasks.Tasks {
		assert.NotEmpty(t, task.Task, task.ID)
		assert.False(t, ids[task.ID], "duplicate task id %s", task.ID)
		ids[task.ID] = true
		assert.True(t, known[task.Expected.Server][task.Expected.Tool], "task %s expects unknown tool %s/%s", task.ID, task.Expected.Server, task.Expected.Tool)
	}
}
