package audit_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/audit"
)

// fakeS3 records puts and deletes and answers the object lock check.
type fakeS3 struct {
	mutex sync.Mutex
	// lockErrors are returned by the first lock checks, in order.
	lockErrors []error
	lockChecks int
	locked     bool
	putErr     error
	puts       []*s3.PutObjectInput
	// objects maps a key to its body, for GetObject.
	objects map[string]string
	// deleteRequests holds the keys of each DeleteObjects request.
	deleteRequests [][]string
	// deleteErrors maps a key to the error code S3 answers for it.
	deleteErrors map[string]string
	// deleteFails fails whole requests.
	deleteFails bool
}

func (f *fakeS3) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	if f.putErr != nil {
		return nil, f.putErr
	}

	f.puts = append(f.puts, input)

	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	body, ok := f.objects[aws.ToString(input.Key)]
	if !ok || aws.ToString(input.Bucket) != "audit" {
		return nil, &types.NoSuchKey{}
	}

	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (f *fakeS3) DeleteObjects(_ context.Context, input *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	var keys []string

	output := &s3.DeleteObjectsOutput{}

	for _, object := range input.Delete.Objects {
		key := aws.ToString(object.Key)
		keys = append(keys, key)

		if code, ok := f.deleteErrors[key]; ok {
			output.Errors = append(output.Errors, types.Error{Key: aws.String(key), Code: aws.String(code)})
		}
	}

	f.deleteRequests = append(f.deleteRequests, keys)

	if f.deleteFails {
		return nil, errors.New("access denied")
	}

	return output, nil
}

func (f *fakeS3) GetObjectLockConfiguration(context.Context, *s3.GetObjectLockConfigurationInput, ...func(*s3.Options)) (*s3.GetObjectLockConfigurationOutput, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()

	f.lockChecks++

	if len(f.lockErrors) > 0 {
		err := f.lockErrors[0]
		f.lockErrors = f.lockErrors[1:]

		return nil, err
	}

	if !f.locked {
		return nil, &smithy.GenericAPIError{Code: "ObjectLockConfigurationNotFoundError"}
	}

	return &s3.GetObjectLockConfigurationOutput{ObjectLockConfiguration: &types.ObjectLockConfiguration{
		ObjectLockEnabled: types.ObjectLockEnabledEnabled,
	}}, nil
}

var payloadNow = time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)

func newTestPayloads(t *testing.T, client *fakeS3) *audit.Payloads {
	t.Helper()

	payloads, err := audit.NewPayloads(t.Context(), client, audit.PayloadOptions{
		Bucket: "audit", Prefix: "tg/", Retention: 24 * time.Hour, Now: func() time.Time { return payloadNow },
	})
	require.NoError(t, err)

	return payloads
}

// sizedPayload is a JSON string payload whose encoding is size bytes.
func sizedPayload(size int) string {
	return strings.Repeat("a", size-2)
}

func TestAttachStoresSmallPayloadsInlineAndLargeOnesInS3(t *testing.T) {
	tests := []struct {
		name       string
		size       int
		wantInline bool
	}{
		{name: "one byte under the limit", size: audit.InlinePayloadLimit - 1, wantInline: true},
		{name: "exactly the limit", size: audit.InlinePayloadLimit, wantInline: false},
		{name: "one byte over the limit", size: audit.InlinePayloadLimit + 1, wantInline: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeS3{}
			payloads := newTestPayloads(t, client)

			event := audit.Event{Type: audit.TypeToolResult}
			require.NoError(t, payloads.Attach(t.Context(), &event, sizedPayload(test.size)))

			if test.wantInline {
				assert.Len(t, event.Payload, test.size)
				assert.Empty(t, event.PayloadRef)
				assert.Empty(t, client.puts)

				return
			}

			assert.Nil(t, event.Payload)
			require.Len(t, client.puts, 1)
			assert.Equal(t, event.PayloadRef, aws.ToString(client.puts[0].Key))
			assert.Equal(t, "audit", aws.ToString(client.puts[0].Bucket))
			assert.Regexp(t, `^tg/payloads/2026/03/07/[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}\.json$`, event.PayloadRef)
			assert.Equal(t, int64(test.size), aws.ToInt64(client.puts[0].ContentLength))
			assert.Empty(t, client.puts[0].ObjectLockMode, "no lock on an unlocked bucket")
		})
	}
}

func TestAttachWithPayloadsOffStoresNothing(t *testing.T) {
	var payloads *audit.Payloads

	event := audit.Event{Type: audit.TypeToolDecision}
	require.NoError(t, payloads.Attach(t.Context(), &event, map[string]any{"text": "hello"}))
	assert.Nil(t, event.Payload)
	assert.Empty(t, event.PayloadRef)
}

func TestAttachLocksObjectsInALockedBucket(t *testing.T) {
	client := &fakeS3{locked: true}
	payloads := newTestPayloads(t, client)
	require.True(t, payloads.Locked())

	event := audit.Event{Type: audit.TypeToolResult}
	require.NoError(t, payloads.Attach(t.Context(), &event, sizedPayload(audit.InlinePayloadLimit)))

	require.Len(t, client.puts, 1)
	put := client.puts[0]
	assert.Equal(t, types.ObjectLockModeGovernance, put.ObjectLockMode)
	assert.Equal(t, payloadNow.Add(24*time.Hour), aws.ToTime(put.ObjectLockRetainUntilDate))
	assert.Equal(t, types.ChecksumAlgorithmSha256, put.ChecksumAlgorithm)
}

func TestAttachCountsFailedPuts(t *testing.T) {
	client := &fakeS3{putErr: errors.New("unavailable")}
	payloads := newTestPayloads(t, client)

	event := audit.Event{Type: audit.TypeToolResult}
	require.Error(t, payloads.Attach(t.Context(), &event, sizedPayload(audit.InlinePayloadLimit)))
	assert.Empty(t, event.PayloadRef)
	assert.InDelta(t, 1, testutil.ToFloat64(payloads.Collectors()[0]), 0)
}

func TestNewPayloadsObjectLockCheck(t *testing.T) {
	t.Run("a not-found answer means no lock", func(t *testing.T) {
		client := &fakeS3{}
		payloads := newTestPayloads(t, client)
		assert.False(t, payloads.Locked())
		assert.Equal(t, 1, client.lockChecks)
	})

	t.Run("other errors are retried", func(t *testing.T) {
		client := &fakeS3{locked: true, lockErrors: []error{errors.New("connection refused")}}
		payloads := newTestPayloads(t, client)
		assert.True(t, payloads.Locked())
		assert.Equal(t, 2, client.lockChecks)
	})

	t.Run("errors past the timeout fail", func(t *testing.T) {
		failures := make([]error, 100)
		for index := range failures {
			failures[index] = fmt.Errorf("connection refused %d", index)
		}

		client := &fakeS3{lockErrors: failures}
		_, err := audit.NewPayloads(t.Context(), client, audit.PayloadOptions{Bucket: "audit", LockCheckTimeout: time.Second})
		require.ErrorContains(t, err, "connection refused")
		assert.GreaterOrEqual(t, client.lockChecks, 2)
	})
}

func TestDeleteObjects(t *testing.T) {
	keys := func(count int) []string {
		list := make([]string, count)
		for index := range list {
			list[index] = fmt.Sprintf("key-%d", index)
		}

		return list
	}

	t.Run("requests hold at most 1000 keys", func(t *testing.T) {
		for _, test := range []struct {
			count     int
			wantSizes []int
		}{
			{count: 1000, wantSizes: []int{1000}},
			{count: 1001, wantSizes: []int{1000, 1}},
		} {
			client := &fakeS3{}
			refused, err := newTestPayloads(t, client).DeleteObjects(t.Context(), keys(test.count))
			require.NoError(t, err)
			assert.Empty(t, refused)

			var sizes []int
			for _, request := range client.deleteRequests {
				sizes = append(sizes, len(request))
			}

			assert.Equal(t, test.wantSizes, sizes)
		}
	})

	t.Run("a missing object counts as deleted; other errors are refused", func(t *testing.T) {
		client := &fakeS3{deleteErrors: map[string]string{"key-0": "NoSuchKey", "key-1": "AccessDenied"}}
		refused, err := newTestPayloads(t, client).DeleteObjects(t.Context(), keys(3))
		require.NoError(t, err)
		assert.Equal(t, map[string]bool{"key-1": true}, refused)
	})

	t.Run("a failed request refuses all of its keys", func(t *testing.T) {
		client := &fakeS3{deleteFails: true}
		refused, err := newTestPayloads(t, client).DeleteObjects(t.Context(), keys(2))
		require.Error(t, err)
		assert.Equal(t, map[string]bool{"key-0": true, "key-1": true}, refused)
	})
}

func TestOpenReadsTheObject(t *testing.T) {
	payloads := newTestPayloads(t, &fakeS3{objects: map[string]string{"tg/payloads/x.json": `{"a":1}`}})

	body, err := payloads.Open(t.Context(), "tg/payloads/x.json")
	require.NoError(t, err)

	defer func() { _ = body.Close() }()

	content, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":1}`, string(content))

	_, err = payloads.Open(t.Context(), "tg/payloads/missing.json")
	require.Error(t, err)

	var off *audit.Payloads

	_, err = off.Open(t.Context(), "tg/payloads/x.json")
	require.Error(t, err)
}
