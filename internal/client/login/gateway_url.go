package login

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// NormalizeGatewayURL turns what a user typed into the gateway's root URL.
// A value without a scheme gets https://. A trailing slash is dropped and the
// host is lowered. A path, query, fragment or user info is refused. Plain
// http is allowed only for localhost and loopback addresses.
func NormalizeGatewayURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("no gateway URL; pass --gateway")
	}

	withScheme := raw
	if !strings.Contains(raw, "://") {
		withScheme = "https://" + raw
	}

	parsed, err := url.Parse(withScheme)
	if err != nil {
		return "", fmt.Errorf("gateway URL %q is not valid: %w", raw, err)
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("gateway URL %q must start with https:// (or be a bare host)", raw)
	}

	hostname := parsed.Hostname()
	if hostname == "" {
		return "", fmt.Errorf("gateway URL %q has no host", raw)
	}

	switch {
	case parsed.User != nil:
		return "", fmt.Errorf("gateway URL %q must not hold a user name or password", raw)
	case parsed.RawQuery != "" || parsed.ForceQuery:
		return "", fmt.Errorf("gateway URL %q must not have a query", raw)
	case parsed.Fragment != "" || strings.Contains(withScheme, "#"):
		return "", fmt.Errorf("gateway URL %q must not have a fragment", raw)
	case parsed.Path != "" && parsed.Path != "/":
		return "", fmt.Errorf("gateway URL %q must not have a path: give the gateway's root, the plugin adds /mcp", raw)
	}

	hostname = strings.ToLower(hostname)

	if parsed.Scheme == "http" && !isLoopbackHost(hostname) {
		return "", fmt.Errorf("gateway URL %q uses plain http; only localhost may, use https://", raw)
	}

	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}

	if port := parsed.Port(); port != "" {
		host = net.JoinHostPort(hostname, port)
	}

	return parsed.Scheme + "://" + host, nil
}

func isLoopbackHost(hostname string) bool {
	if hostname == "localhost" {
		return true
	}

	ip := net.ParseIP(hostname)

	return ip != nil && ip.IsLoopback()
}
