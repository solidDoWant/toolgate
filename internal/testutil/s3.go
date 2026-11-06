package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// S3Image is the S3-compatible server the integration tests run: the Versity
// S3 Gateway with its POSIX backend, pinned by digest.
const S3Image = "ghcr.io/versity/versitygw:v1.10.0@sha256:89cad13cc981986ee284cbafea4613f9533ce6fd5f37a2c96c4c4fb8adb3a316"

const (
	s3Region = "us-east-1"
	s3Port   = "7070/tcp"
)

// S3 is a running S3-compatible server with one bucket.
type S3 struct {
	// ContainerID is the Docker container running the server.
	ContainerID string
	// Endpoint is the base URL of the server. Clients must use path-style
	// addressing.
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	// Bucket exists and is empty when StartS3 returns.
	Bucket string
}

// StartS3 starts an S3-compatible server in Docker, creates a bucket and
// removes the server when tb ends. It skips tb when Docker is not available.
func StartS3(tb testing.TB) *S3 {
	tb.Helper()
	RequireDocker(tb)

	ctx := tb.Context()
	accessKey := randomHex(tb, 10)
	secretKey := randomHex(tb, 20)

	container, err := testcontainers.Run(ctx, S3Image,
		testcontainers.WithEntrypoint("sh", "-c",
			// The sidecar directory keeps object metadata in files, so the
			// gateway does not need extended attributes on the container's
			// filesystem.
			`mkdir -p /data /meta && exec versitygw --port :7070 posix --sidecar /meta /data`),
		testcontainers.WithEnv(map[string]string{
			"ROOT_ACCESS_KEY_ID":     accessKey,
			"ROOT_SECRET_ACCESS_KEY": secretKey,
			"VGW_REGION":             s3Region,
		}),
		testcontainers.WithExposedPorts(s3Port),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/").WithPort(s3Port).
				// Any S3 answer, including "access denied" for the anonymous
				// probe, means the gateway is serving.
				WithStatusCodeMatcher(func(int) bool { return true }).
				WithStartupTimeout(2*time.Minute),
		),
	)
	testcontainers.CleanupContainer(tb, container)
	require.NoError(tb, err, "start S3 server")

	endpoint, err := container.PortEndpoint(ctx, s3Port, "http")
	require.NoError(tb, err)

	server := &S3{
		ContainerID:     container.GetContainerID(),
		Endpoint:        endpoint,
		Region:          s3Region,
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
		Bucket:          "toolgate-test",
	}

	_, err = server.Client().CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(server.Bucket)})
	require.NoError(tb, err, "create bucket %q", server.Bucket)

	return server
}

// Client returns an S3 client for the server, authenticated as its root user.
func (s *S3) Client() *s3.Client {
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(s.Endpoint),
		Region:       s.Region,
		Credentials:  credentials.NewStaticCredentialsProvider(s.AccessKeyID, s.SecretAccessKey, ""),
		UsePathStyle: true,
	})
}

// CreateBucket creates another bucket.
func (s *S3) CreateBucket(ctx context.Context, name string) error {
	_, err := s.Client().CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)})

	return err
}

func randomHex(tb testing.TB, size int) string {
	tb.Helper()

	buffer := make([]byte, size)
	_, err := rand.Read(buffer)
	require.NoError(tb, err)

	return hex.EncodeToString(buffer)
}
