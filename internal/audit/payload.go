package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// InlinePayloadLimit is the size in bytes from which a payload is stored
	// in S3 instead of in its audit row (docs/design/phase-2.md §8).
	InlinePayloadLimit = 65536
	// MaxDeleteKeys is the most keys one DeleteObjects request takes.
	MaxDeleteKeys = 1000
	// DefaultLockCheckTimeout is how long NewPayloads retries reading the
	// bucket's object lock configuration.
	DefaultLockCheckTimeout = 30 * time.Second
)

// S3API is the part of the S3 client that payloads use. *s3.Client
// implements it.
type S3API interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
	GetObjectLockConfiguration(ctx context.Context, params *s3.GetObjectLockConfigurationInput, optFns ...func(*s3.Options)) (*s3.GetObjectLockConfigurationOutput, error)
}

// Payloads stores audit payloads: small ones inline in the row, large ones
// as S3 objects. A nil *Payloads means payloads are off: nothing is stored.
type Payloads struct {
	client    S3API
	bucket    string
	prefix    string
	retention time.Duration
	locked    bool
	now       func() time.Time
	errors    prometheus.Counter
}

// PayloadOptions configure NewPayloads.
type PayloadOptions struct {
	Bucket string
	Prefix string
	// Retention is how long rows are kept; locked objects are retained this
	// long. Zero means DefaultRetention.
	Retention time.Duration
	// LockCheckTimeout bounds the retries of the object lock check. Zero
	// means DefaultLockCheckTimeout.
	LockCheckTimeout time.Duration
	// Now returns the current time; it defaults to time.Now.
	Now func() time.Time
}

// NewPayloads reads the bucket's object lock configuration and returns a
// payload store for it. A not-found answer means the bucket has no lock;
// other errors are retried until LockCheckTimeout passes.
func NewPayloads(ctx context.Context, client S3API, options PayloadOptions) (*Payloads, error) {
	if options.Retention <= 0 {
		options.Retention = DefaultRetention
	}

	if options.LockCheckTimeout <= 0 {
		options.LockCheckTimeout = DefaultLockCheckTimeout
	}

	if options.Now == nil {
		options.Now = time.Now
	}

	locked, err := objectLockEnabled(ctx, client, options.Bucket, options.LockCheckTimeout)
	if err != nil {
		return nil, err
	}

	return &Payloads{
		client:    client,
		bucket:    options.Bucket,
		prefix:    options.Prefix,
		retention: options.Retention,
		locked:    locked,
		now:       options.Now,
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "toolgate_audit_payload_errors_total",
			Help: "Audit payloads that could not be stored.",
		}),
	}, nil
}

func objectLockEnabled(ctx context.Context, client S3API, bucket string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	delay := 500 * time.Millisecond

	for {
		output, err := client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String(bucket)})
		if err == nil {
			return output.ObjectLockConfiguration != nil &&
				output.ObjectLockConfiguration.ObjectLockEnabled == types.ObjectLockEnabledEnabled, nil
		}

		var apiError smithy.APIError
		if errors.As(err, &apiError) && apiError.ErrorCode() == "ObjectLockConfigurationNotFoundError" {
			return false, nil
		}

		if time.Now().Add(delay).After(deadline) {
			return false, fmt.Errorf("read the object lock configuration of bucket %s: %w", bucket, err)
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(delay):
		}

		delay = min(2*delay, 5*time.Second)
	}
}

// Locked reports whether the bucket has object lock enabled.
func (p *Payloads) Locked() bool {
	return p != nil && p.locked
}

// Collectors returns the payload metrics to register.
func (p *Payloads) Collectors() []prometheus.Collector {
	if p == nil {
		return nil
	}

	return []prometheus.Collector{p.errors}
}

// Attach stores payload for event: as JSON in event.Payload when it is
// under InlinePayloadLimit, otherwise as one S3 object whose key goes in
// event.PayloadRef. Call it before Record, so the object exists before the
// row that refers to it. With payloads off (a nil receiver) or a nil
// payload it does nothing. An error means the payload wasn't stored and is
// counted in toolgate_audit_payload_errors_total.
func (p *Payloads) Attach(ctx context.Context, event *Event, payload any) error {
	if p == nil || payload == nil {
		return nil
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		p.errors.Inc()

		return fmt.Errorf("encode audit payload: %w", err)
	}

	if len(encoded) < InlinePayloadLimit {
		event.Payload = encoded

		return nil
	}

	now := p.now().UTC()

	id, err := uuid.NewV7()
	if err != nil {
		p.errors.Inc()

		return fmt.Errorf("create audit payload key: %w", err)
	}

	key := fmt.Sprintf("%spayloads/%04d/%02d/%02d/%s.json", p.prefix, now.Year(), int(now.Month()), now.Day(), id)

	input := &s3.PutObjectInput{
		Bucket:        aws.String(p.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(encoded),
		ContentLength: aws.Int64(int64(len(encoded))),
		ContentType:   aws.String("application/json"),
	}

	if p.locked {
		input.ObjectLockMode = types.ObjectLockModeGovernance
		input.ObjectLockRetainUntilDate = aws.Time(now.Add(p.retention))
		input.ChecksumAlgorithm = types.ChecksumAlgorithmSha256
	}

	if _, err := p.client.PutObject(ctx, input); err != nil {
		p.errors.Inc()

		return fmt.Errorf("store audit payload: %w", err)
	}

	event.PayloadRef = key

	return nil
}

// DeleteObjects deletes the objects with keys, at most MaxDeleteKeys a
// request, and returns the keys S3 refused to delete. A missing object
// counts as deleted. When a whole request fails, all of its keys are
// refused and the error is returned with them.
func (p *Payloads) DeleteObjects(ctx context.Context, keys []string) (map[string]bool, error) {
	refused := map[string]bool{}

	var requestErrors []error

	for start := 0; start < len(keys); start += MaxDeleteKeys {
		batch := keys[start:min(start+MaxDeleteKeys, len(keys))]

		objects := make([]types.ObjectIdentifier, 0, len(batch))
		for _, key := range batch {
			objects = append(objects, types.ObjectIdentifier{Key: aws.String(key)})
		}

		output, err := p.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(p.bucket),
			Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		})
		if err != nil {
			for _, key := range batch {
				refused[key] = true
			}

			requestErrors = append(requestErrors, err)

			continue
		}

		for _, failure := range output.Errors {
			if aws.ToString(failure.Code) == "NoSuchKey" {
				continue
			}

			refused[aws.ToString(failure.Key)] = true
		}
	}

	if len(requestErrors) > 0 {
		return refused, fmt.Errorf("delete audit payload objects: %w", errors.Join(requestErrors...))
	}

	return refused, nil
}
