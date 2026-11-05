package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// newAuditPayloads returns the audit payload store, or nil when
// auditPayloads is off. It reads the bucket's object lock configuration, so
// an unreachable bucket stops the gateway at startup.
func newAuditPayloads(ctx context.Context, cfg config.Config, server *gateway.Server) (*audit.Payloads, error) {
	if !cfg.AuditPayloads {
		return nil, nil
	}

	awsConfig, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AuditS3Region))
	if err != nil {
		return nil, fmt.Errorf("audit payloads: load S3 configuration: %w", err)
	}

	client := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		if cfg.AuditS3Endpoint != "" {
			options.BaseEndpoint = aws.String(cfg.AuditS3Endpoint)
		}

		options.UsePathStyle = cfg.AuditS3PathStyle
	})

	payloads, err := audit.NewPayloads(ctx, client, audit.PayloadOptions{
		Bucket:    cfg.AuditS3Bucket,
		Prefix:    cfg.AuditS3Prefix,
		Retention: audit.DefaultRetention,
	})
	if err != nil {
		return nil, fmt.Errorf("audit payloads: %w", err)
	}

	if err := server.RegisterMetrics(payloads.Collectors()...); err != nil {
		return nil, err
	}

	return payloads, nil
}

// startAuditRetention runs the audit retention job until ctx is done. With
// payloads on, it deletes payload objects before their rows.
func startAuditRetention(ctx context.Context, server *gateway.Server, payloads *audit.Payloads, logger *slog.Logger) error {
	queries := db.New(server.Database())
	retention := audit.NewRetention(queries, audit.DefaultRetention, logger)

	if payloads != nil {
		retention.Rows = queries
		retention.Objects = payloads
	}

	if err := server.RegisterMetrics(retention.Refused); err != nil {
		return err
	}

	go retention.Run(ctx)

	return nil
}
