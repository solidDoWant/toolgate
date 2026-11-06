package testutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serverWidePatterns are statements that change or reach beyond the one
// database a test owns on the shared server. A function that runs one must
// either use a dedicated server or show how it stays inside its own database.
var serverWidePatterns = []struct {
	statement string
	// safeMarkers are text that makes the statement safe on the shared
	// server.
	safeMarkers []string
}{
	{"alter system", nil},
	{"create role", []string{"RoleName("}},
	{"pg_terminate_backend", []string{"current_database()"}},
}

// serverWideViolations returns the functions of the Go source in src that run
// a server-wide statement without a dedicated server or a safe marker.
func serverWideViolations(t *testing.T, name string, src []byte) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	require.NoError(t, err)

	var found []string

	for _, decl := range file.Decls {
		function, ok := decl.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}

		body := string(src[fset.Position(function.Body.Pos()).Offset:fset.Position(function.Body.End()).Offset])
		if strings.Contains(body, "StartPostgresDedicated(") {
			continue
		}

		lower := strings.ToLower(body)

		for _, pattern := range serverWidePatterns {
			if !strings.Contains(lower, pattern.statement) {
				continue
			}

			safe := false

			for _, marker := range pattern.safeMarkers {
				safe = safe || strings.Contains(body, marker)
			}

			if !safe {
				found = append(found, name+": "+function.Name.Name+" runs "+pattern.statement)
			}
		}
	}

	return found
}

func TestServerWideViolationsFindsUnsafeStatements(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"alter system on the shared server", `pg := StartPostgres(t); pool.Exec(ctx, "ALTER SYSTEM SET x = 1")`, 1},
		{"alter system on a dedicated server", `pg := StartPostgresDedicated(t); pool.Exec(ctx, "ALTER SYSTEM SET x = 1")`, 0},
		{"unprefixed role", `pool.Exec(ctx, "CREATE ROLE reader LOGIN")`, 1},
		{"prefixed role", `pg.RoleName("reader"); pool.Exec(ctx, "CREATE ROLE x LOGIN")`, 0},
		{"terminate across databases", `pool.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity")`, 1},
		{"terminate in this database", `pool.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = current_database()")`, 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			src := "package x\nfunc TestX(t *testing.T) {\n" + test.body + "\n}\n"

			assert.Len(t, serverWideViolations(t, "x_test.go", []byte(src)), test.want)
		})
	}
}

// TestSharedServerTestsStayInsideTheirDatabase checks every test file of the
// repository.
func TestSharedServerTestsStayInsideTheirDatabase(t *testing.T) {
	root := filepath.Join("..", "..")

	var violations []string

	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		name := entry.Name()
		if entry.IsDir() {
			// The end-to-end tier uses PostgreSQL in the kind cluster.
			if (strings.HasPrefix(name, ".") && path != root) || name == "e2e" || name == "node_modules" {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(name, "_test.go") || name == "postgres_guard_test.go" {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		violations = append(violations, serverWideViolations(t, path, src)...)

		return nil
	}))

	assert.Empty(t, violations, "use StartPostgresDedicated, or PostgreSQL's RoleName and current_database()")
}
