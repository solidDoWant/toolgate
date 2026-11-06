package testutil

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// PostgresImage is the PostgreSQL image the integration tests run, pinned by
// digest. It ships the pgvector and pg_trgm extensions.
const PostgresImage = "ghcr.io/cloudnative-pg/postgresql:17-standard-trixie@sha256:4d0aac315c4d8c3202252bd06fea83b16951cc57234a7b610eeaa382ff4482d0"

const (
	postgresUser     = "toolgate"
	postgresDatabase = "toolgate"
	postgresTLSDir   = "/tmp/postgresql-tls"
)

// postgresHBA accepts TCP connections only over TLS with a client
// certificate whose common name is the database user. The local socket is
// trusted so the image's entrypoint can initialise the database.
const postgresHBA = `local all all trust
hostssl all all all cert
`

// postgresStart initialises a database cluster, creates the database, copies
// the TLS files to a directory the postgres user owns (PostgreSQL refuses a
// key file readable by others; the test files are readable by the
// container user, which is not root) and starts the server with TLS on. The image
// has no entrypoint script, so this does what the official image's does.
const postgresStart = `set -eu
mkdir -p ` + postgresTLSDir + `
cp /tls/* ` + postgresTLSDir + `/
chmod 0600 ` + postgresTLSDir + `/server.key
export PGDATA=/tmp/pgdata
initdb -D "$PGDATA" -U ` + postgresUser + ` --auth=trust >/dev/null
export PGHOST=/tmp
pg_ctl -D "$PGDATA" -w -o "-c listen_addresses='' -c unix_socket_directories=/tmp" start >/dev/null
createdb -U ` + postgresUser + ` ` + postgresDatabase + `
pg_ctl -D "$PGDATA" -w stop >/dev/null
exec postgres -D "$PGDATA" \
  -c listen_addresses='*' \
  -c ssl=on \
  -c ssl_cert_file=` + postgresTLSDir + `/server.crt \
  -c ssl_key_file=` + postgresTLSDir + `/server.key \
  -c ssl_ca_file=` + postgresTLSDir + `/ca.crt \
  -c hba_file=` + postgresTLSDir + `/pg_hba.conf
`

// Postgres is a running PostgreSQL server that requires TLS
// client-certificate authentication.
type Postgres struct {
	// ContainerID is the Docker container running the server.
	ContainerID string

	Host     string
	Port     string
	User     string
	Database string

	// CAFile verifies the server certificate. CertFile and KeyFile are the
	// client certificate for User.
	CAFile   string
	CertFile string
	KeyFile  string

	// CA signs the server and client certificates; use it to issue more
	// client certificates.
	CA *x509.Certificate

	// Dedicated is true when the server belongs to one test. Otherwise other
	// tests share it and Database is this test's own database.
	Dedicated bool

	ca *certAuthority
}

// StartPostgres returns a fresh, migrated database on the PostgreSQL server
// this test binary shares. The server starts on the first call and lives
// until the binary exits. The database is cloned from a template the binary
// migrates once, and it is dropped when tb ends. It skips tb when Docker is
// not available.
//
// Tests that restart or stop the server, kill connections server-wide, change
// server settings, or need a server of their own use StartPostgresDedicated.
func StartPostgres(tb testing.TB) *Postgres {
	tb.Helper()

	return startSharedDatabase(tb, sharedMigratedTemplate)
}

// StartPostgresEmpty is like StartPostgres, but the database has no tables.
// Use it for tests of the migrations themselves.
func StartPostgresEmpty(tb testing.TB) *Postgres {
	tb.Helper()

	return startSharedDatabase(tb, sharedEmptyTemplate)
}

// StartPostgresDedicated starts a PostgreSQL server for tb alone, with an empty
// database, and removes it when tb ends. It skips tb when Docker is not
// available.
func StartPostgresDedicated(tb testing.TB) *Postgres {
	tb.Helper()
	RequireDocker(tb)

	pg, container, err := startPostgresServer(tb.Context(), tb)
	testcontainers.CleanupContainer(tb, container)
	require.NoError(tb, err, "start PostgreSQL")

	pg.Dedicated = true

	return pg
}

// startPostgresServer starts one container and returns a Postgres for its
// default database. Its files live in a directory that stays until the
// process exits when tb is nil, and until tb ends otherwise.
func startPostgresServer(ctx context.Context, tb testing.TB) (*Postgres, testcontainers.Container, error) {
	host, err := daemonHost(ctx)
	if err != nil {
		return nil, nil, err
	}

	ca, err := newCertAuthorityE()
	if err != nil {
		return nil, nil, err
	}

	server, err := ca.issueE("localhost", x509.ExtKeyUsageServerAuth, uniqueHosts("localhost", "127.0.0.1", "::1", host)...)
	if err != nil {
		return nil, nil, err
	}

	client, err := ca.issueE(postgresUser, x509.ExtKeyUsageClientAuth)
	if err != nil {
		return nil, nil, err
	}

	files := map[string][]byte{
		"ca.crt":      ca.certPEM,
		"server.crt":  server.CertPEM,
		"server.key":  server.KeyPEM,
		"pg_hba.conf": []byte(postgresHBA),
	}

	containerFiles := make([]testcontainers.ContainerFile, 0, len(files))
	for name, content := range files {
		containerFiles = append(containerFiles, testcontainers.ContainerFile{
			Reader:            bytes.NewReader(content),
			ContainerFilePath: "/tls/" + name,
			FileMode:          0o644,
		})
	}

	container, err := testcontainers.Run(ctx, PostgresImage,
		testcontainers.WithFiles(containerFiles...),
		testcontainers.WithEntrypoint("sh", "-c", postgresStart),
		testcontainers.WithCmd(),
		testcontainers.WithExposedPorts("5432/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithStartupTimeout(2*time.Minute),
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(2*time.Minute),
		),
	)
	if err != nil {
		return nil, container, err
	}

	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return nil, container, err
	}

	var dir string
	if tb != nil {
		dir = tb.TempDir()
	} else if dir, err = os.MkdirTemp("", "toolgate-postgres-"); err != nil {
		return nil, container, err
	}

	pg := &Postgres{
		ContainerID: container.GetContainerID(),
		Host:        host,
		Port:        port.Port(),
		User:        postgresUser,
		Database:    postgresDatabase,
		CA:          ca.cert,
		ca:          ca,
	}

	for _, file := range []struct {
		name    string
		content []byte
		path    *string
	}{
		{"ca.crt", ca.certPEM, &pg.CAFile},
		{"client.crt", client.CertPEM, &pg.CertFile},
		{"client.key", client.KeyPEM, &pg.KeyFile},
	} {
		path := filepath.Join(dir, file.name)
		if err := os.WriteFile(path, file.content, 0o600); err != nil {
			return nil, container, err
		}

		*file.path = path
	}

	return pg, container, nil
}

// ConnString returns a libpq-style URL that authenticates with the client
// certificate and verifies the server certificate.
func (p *Postgres) ConnString() string {
	return p.connString(p.CertFile, p.KeyFile)
}

// ConnStringWithoutClientCert returns a URL that verifies the server but
// presents no client certificate. The server refuses it.
func (p *Postgres) ConnStringWithoutClientCert() string {
	return p.connString("", "")
}

// IssueClientCert issues a client certificate for user, signed by the CA the
// server trusts, and returns the paths of the certificate and key files.
func (p *Postgres) IssueClientCert(tb testing.TB, user string) (certFile, keyFile string) {
	tb.Helper()

	pair := p.ca.issue(tb, user, x509.ExtKeyUsageClientAuth)
	dir := tb.TempDir()

	return writeFile(tb, dir, "client.crt", pair.CertPEM), writeFile(tb, dir, "client.key", pair.KeyPEM)
}

// ConnStringFor returns a URL that authenticates as user with the given
// client certificate files.
func (p *Postgres) ConnStringFor(user, certFile, keyFile string) string {
	return p.buildConnString(user, certFile, keyFile)
}

func (p *Postgres) connString(certFile, keyFile string) string {
	return p.buildConnString(p.User, certFile, keyFile)
}

func (p *Postgres) buildConnString(user, certFile, keyFile string) string {
	query := url.Values{}
	query.Set("sslmode", "verify-full")
	query.Set("sslrootcert", p.CAFile)

	if certFile != "" {
		query.Set("sslcert", certFile)
		query.Set("sslkey", keyFile)
	}

	connURL := url.URL{
		Scheme:   "postgres",
		User:     url.User(user),
		Host:     hostPort(p.Host, p.Port),
		Path:     "/" + p.Database,
		RawQuery: query.Encode(),
	}

	return connURL.String()
}

func hostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return fmt.Sprintf("[%s]:%s", host, port)
	}

	return host + ":" + port
}

func daemonHost(ctx context.Context) (string, error) {
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return "", err
	}

	defer func() { _ = provider.Close() }()

	return provider.DaemonHost(ctx)
}

func uniqueHosts(hosts ...string) []string {
	seen := make(map[string]bool, len(hosts))

	unique := make([]string, 0, len(hosts))
	for _, host := range hosts {
		if host == "" || seen[host] {
			continue
		}

		seen[host] = true
		unique = append(unique, host)
	}

	return unique
}

func writeFile(tb testing.TB, dir, name string, content []byte) string {
	tb.Helper()

	path := filepath.Join(dir, name)
	require.NoError(tb, os.WriteFile(path, content, 0o600))

	return path
}
