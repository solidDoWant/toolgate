package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testtime"
)

// Custom health check defaults and limits. The
// CRD sets the defaults; these cover a stored check that lacks them.
const (
	defaultCheckInterval         = 30 * time.Second
	defaultCheckTimeout          = 5 * time.Second
	defaultCheckFailureThreshold = 3
	defaultCheckSuccessThreshold = 1
	defaultCheckExpectedStatus   = http.StatusOK
	// MaxHealthCheckBody is the most of a check's response body that is
	// read before it is discarded.
	MaxHealthCheckBody = 64 << 10
)

// HealthProbe makes one health check request and returns the status code
// of the answer.
type HealthProbe func(ctx context.Context, url string) (int, error)

// healthCheckClient doesn't follow redirects and uses no proxy.
var healthCheckClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     90 * time.Second,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// HTTPHealthProbe is the HealthProbe that sends GET url. A redirect is not
// followed, and at most MaxHealthCheckBody bytes of the body are read.
func HTTPHealthProbe(ctx context.Context, url string) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	response, err := healthCheckClient.Do(request)
	if err != nil {
		return 0, err
	}

	defer func() { _ = response.Body.Close() }()

	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, MaxHealthCheckBody))

	return response.StatusCode, nil
}

// storedHealthCheck is an instance's custom health check, read from its
// health_check column, with defaults filled in.
type storedHealthCheck struct {
	v1alpha1.HealthCheck
}

// decodeHealthCheck parses an instance's health_check column. It returns
// false when the instance has no custom check.
func decodeHealthCheck(encoded []byte) (storedHealthCheck, bool, error) {
	if len(encoded) == 0 || string(encoded) == "null" {
		return storedHealthCheck{}, false, nil
	}

	var check storedHealthCheck
	if err := json.Unmarshal(encoded, &check.HealthCheck); err != nil {
		return storedHealthCheck{}, false, fmt.Errorf("decode health check: %w", err)
	}

	if check.Interval.Duration <= 0 {
		check.Interval.Duration = defaultCheckInterval
	}

	// The interval paces the checks and the reapply recheck. The timeout
	// bounds a real request and isn't scaled.
	check.Interval.Duration = testtime.Scale(check.Interval.Duration)

	if check.Timeout.Duration <= 0 {
		check.Timeout.Duration = defaultCheckTimeout
	}

	if check.FailureThreshold <= 0 {
		check.FailureThreshold = defaultCheckFailureThreshold
	}

	if check.SuccessThreshold <= 0 {
		check.SuccessThreshold = defaultCheckSuccessThreshold
	}

	if check.HTTP.ExpectedStatus == 0 {
		check.HTTP.ExpectedStatus = defaultCheckExpectedStatus
	}

	return check, true, nil
}

// url is the address the check requests, in the instance namespace.
func (c storedHealthCheck) url(namespace string) string {
	host := net.JoinHostPort(c.HTTP.Service+"."+namespace+".svc", strconv.Itoa(int(c.HTTP.Port)))

	return "http://" + host + c.HTTP.Path
}

// shownURL is the address as the admin wrote it, used in reasons.
func (c storedHealthCheck) shownURL() string {
	return "http://" + net.JoinHostPort(c.HTTP.Service, strconv.Itoa(int(c.HTTP.Port))) + c.HTTP.Path
}

// healthChecker runs one instance's custom health check, with its own
// ticker and counters, apart from the MCP ping.
type healthChecker struct {
	monitor  *Monitor
	instance db.Instance
	check    storedHealthCheck
	failures int32
	passes   int32
}

// run checks the instance at once and then every interval until it is no
// longer ready or ctx ends.
func (h *healthChecker) run(ctx context.Context) {
	ticker := time.NewTicker(h.check.Interval.Duration)
	defer ticker.Stop()

	for {
		if !h.step(ctx) {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// step runs one check and records a change of health. It returns false
// when checking ends.
func (h *healthChecker) step(ctx context.Context) bool {
	current, err := h.monitor.Store.Instance(ctx, h.instance.ID)
	if err != nil {
		if ctx.Err() == nil {
			h.monitor.logger().WarnContext(ctx, "health check: can't read instance",
				slog.String("instance", h.instance.ID), slog.String("error", err.Error()))
		}

		return ctx.Err() == nil
	}

	if current.Desired != "running" || current.Status != StatusReady {
		return false
	}

	unhealthy := current.UnhealthySince.Valid

	reason := h.probe(ctx)
	if ctx.Err() != nil {
		return false
	}

	if reason == "" {
		h.failures = 0
		h.passes++

		// Only clear an unhealthy state this check set. One set by object
		// health is cleared when the object is ready again.
		if unhealthy && isCheckReason(current.UnhealthyReason) && h.passes >= h.check.SuccessThreshold {
			h.passes = 0
			h.setHealth(ctx, current, true, "")
		}

		return true
	}

	h.passes = 0
	h.failures++
	h.monitor.logger().InfoContext(ctx, "custom health check failed", slog.String("instance", h.instance.ID),
		slog.Int("failures", int(h.failures)), slog.String("error", reason))

	if !unhealthy && h.failures >= h.check.FailureThreshold {
		h.failures = 0
		h.setHealth(ctx, current, false, reason)
	}

	return true
}

// probe runs one request and returns why it failed, or "" when it passed.
func (h *healthChecker) probe(ctx context.Context) string {
	probeCtx, cancel := context.WithTimeout(ctx, h.check.Timeout.Duration)
	defer cancel()

	probe := h.monitor.Probe
	if probe == nil {
		probe = HTTPHealthProbe
	}

	status, err := probe(probeCtx, h.check.url(h.instance.Namespace))
	if err != nil {
		return fmt.Sprintf(checkReasonPrefix+"%s failed: %v", h.check.shownURL(), err)
	}

	if status != int(h.check.HTTP.ExpectedStatus) {
		return fmt.Sprintf(checkReasonPrefix+"%s answered %d, expected %d", h.check.shownURL(), status, h.check.HTTP.ExpectedStatus)
	}

	return ""
}

func (h *healthChecker) setHealth(ctx context.Context, current db.Instance, healthy bool, reason string) {
	if _, err := h.monitor.Store.SetHealth(ctx, current, healthy, truncate(reason, maxReasonBytes)); err != nil {
		h.monitor.logger().WarnContext(ctx, "recording instance health", slog.String("instance", h.instance.ID),
			slog.String("error", err.Error()))
	}
}

// checkReasonPrefix starts every unhealthy reason the custom check writes.
// It tells those reasons apart from the ones object health writes.
const checkReasonPrefix = "GET "

// isCheckReason reports whether reason was written by the custom check.
func isCheckReason(reason string) bool {
	return strings.HasPrefix(reason, checkReasonPrefix)
}
