package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/client/headers"
	"github.com/solidDoWant/toolgate/internal/client/sessiontitle"
	"github.com/solidDoWant/toolgate/internal/telemetry/titlemetric"
)

const (
	// titleHookTimeout bounds the Stop and SessionEnd hooks: two sends and
	// a token refresh, inside the plugin's 10 s hook timeout.
	titleHookTimeout = 8 * time.Second
	// titleSendTimeout bounds one send of a title.
	titleSendTimeout = 3 * time.Second
)

// titleHookInput is the part of the Stop and SessionEnd hook input the hooks
// read.
type titleHookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
}

// telemetryOn reports whether Claude Code's metrics go to OTLP in this
// session, from the env that Claude Code passes to its hooks.
func telemetryOn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CLAUDE_CODE_ENABLE_TELEMETRY"))) {
	case "1", "true":
	default:
		return false
	}

	exporters := strings.Split(os.Getenv("OTEL_METRICS_EXPORTER"), ",")
	for index := range exporters {
		exporters[index] = strings.TrimSpace(exporters[index])
	}

	return slices.Contains(exporters, "otlp")
}

// sendSessionTitle sends the session's title to the gateway's OTLP ingest
// when it is due. Every failure is dropped: the next Stop tries again.
func sendSessionTitle(ctx context.Context, stdin io.Reader, now time.Time) {
	if !telemetryOn() {
		return
	}

	content, err := io.ReadAll(io.LimitReader(stdin, maxHookInputBytes))
	if err != nil {
		return
	}

	var input titleHookInput
	if json.Unmarshal(content, &input) != nil || !sessiontitle.ValidSessionID(input.SessionID) || input.TranscriptPath == "" {
		return
	}

	runtimeDir, err := headers.RuntimeDir()
	if err != nil {
		return
	}

	store := sessiontitle.Store{Dir: filepath.Join(runtimeDir, "titles")}
	last, found := store.Last(input.SessionID)

	// A session never sent before may be a resumed one whose title is older
	// than the end of the file, so it gets one read of the whole file.
	title := sessiontitle.Read(input.TranscriptPath, input.SessionID, false)
	if title == "" && !found {
		title = sessiontitle.Read(input.TranscriptPath, input.SessionID, true)
	}

	if title == "" || !sessiontitle.Due(last, found, title, now) {
		return
	}

	body, err := pmetricotlp.NewExportRequestFromMetrics(titlemetric.Metrics(input.SessionID, title, now)).MarshalProto()
	if err != nil {
		return
	}

	workingDir := input.Cwd
	if workingDir == "" {
		if workingDir, err = os.Getwd(); err != nil {
			return
		}
	}

	workspace, err := headers.Workspace(ctx, workingDir)
	if err != nil {
		return
	}

	err = withAccess(ctx, func(access gatewayAccess) error {
		return postTitle(ctx, access, workspace, body)
	})
	if err != nil {
		return
	}

	_ = store.Record(input.SessionID, title, now)
}

// postTitle sends one encoded metrics request to the gateway's /v1/metrics.
func postTitle(ctx context.Context, access gatewayAccess, workspace string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, titleSendTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, access.gateway+"/v1/metrics", bytes.NewReader(body))
	if err != nil {
		return err
	}

	setCredentials(request.Header, access.accessToken, access.attestation)
	request.Header.Set(auth.WorkspaceHeader, workspace)
	request.Header.Set("Content-Type", "application/x-protobuf")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}

	defer func() { _ = response.Body.Close() }()

	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBody))

	if response.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return &gatewayError{Status: response.StatusCode, Message: http.StatusText(response.StatusCode)}
	}

	return nil
}
