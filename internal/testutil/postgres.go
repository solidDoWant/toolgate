package testutil

import (
	"bytes"
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

	ca *certAuthority
}

// StartPostgres starts PostgreSQL in Docker and removes it when tb ends. It
// skips tb when Docker is not available.
func StartPostgres(tb testing.TB) *Postgres {
	tb.Helper()
	RequireDocker(tb)

	ctx := tb.Context()
	host := daemonHost(tb)

	ca := newCertAuthority(tb)
	server := ca.issue(tb, "localhost", x509.ExtKeyUsageServerAuth, uniqueHosts("localhost", "127.0.0.1", "::1", host)...)
	client := ca.issue(tb, postgresUser, x509.ExtKeyUsageClientAuth)

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
	testcontainers.CleanupContainer(tb, container)
	require.NoError(tb, err, "start PostgreSQL")

	port, err := container.MappedPort(ctx, "5432/tcp")
	require.NoError(tb, err)

	dir := tb.TempDir()
	pg := &Postgres{
		ContainerID: container.GetContainerID(),
		Host:        host,
		Port:        port.Port(),
		User:        postgresUser,
		Database:    postgresDatabase,
		CAFile:      writeFile(tb, dir, "ca.crt", ca.certPEM),
		CertFile:    writeFile(tb, dir, "client.crt", client.CertPEM),
		KeyFile:     writeFile(tb, dir, "client.key", client.KeyPEM),
		CA:          ca.cert,
		ca:          ca,
	}

	return pg
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

func daemonHost(tb testing.TB) string {
	tb.Helper()

	provider, err := testcontainers.NewDockerProvider()
	require.NoError(tb, err)

	defer func() { _ = provider.Close() }()

	host, err := provider.DaemonHost(tb.Context())
	require.NoError(tb, err)

	return host
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
