//go:build integration

package testutil_test

import (
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestStartS3(t *testing.T) {
	t.Parallel()

	server := testutil.StartS3(t)

	t.Run("objects round-trip", func(t *testing.T) {
		client := server.Client()

		_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String(server.Bucket),
			Key:    aws.String("payloads/one"),
			Body:   strings.NewReader("hello"),
		})
		require.NoError(t, err)

		object, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String(server.Bucket),
			Key:    aws.String("payloads/one"),
		})
		require.NoError(t, err)

		defer func() { _ = object.Body.Close() }()

		body, err := io.ReadAll(object.Body)
		require.NoError(t, err)
		assert.Equal(t, "hello", string(body))
	})

	t.Run("wrong credentials are refused", func(t *testing.T) {
		client := s3.New(s3.Options{
			BaseEndpoint: aws.String(server.Endpoint),
			Region:       server.Region,
			Credentials:  credentials.NewStaticCredentialsProvider(server.AccessKeyID, "wrong", ""),
			UsePathStyle: true,
		})

		_, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(server.Bucket)})
		require.Error(t, err)
	})
}
