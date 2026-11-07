package catalog

import (
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// Remote request timeout bounds. The CRD's CEL rules check the same ones.
const (
	minRemoteTimeout = time.Second
	maxRemoteTimeout = 5 * time.Minute
)

// clusterServiceSuffixes are the host suffixes plain http:// may reach: a
// Service's in-cluster DNS names, whose traffic stays inside the cluster.
var clusterServiceSuffixes = []string{".svc", ".svc.cluster.local"}

// validateRemote checks the template's remote server, if any: its URL and
// the variables in its header values. A problem names the header but never
// quotes its value, so no part of a value reaches the status or the log.
func validateRemote(remote *v1alpha1.RemoteServer) []string {
	if remote == nil {
		return nil
	}

	var problems []string

	if problem := remoteURLProblem(remote.URL); problem != "" {
		problems = append(problems, problem)
	}

	var headerProblems []string

	for name, value := range remote.Headers {
		if !headerValueAllowed(value) {
			headerProblems = append(headerProblems, "The value of header `"+name+"` in `remote.headers` may use only ${secret:NAME} and ${principal.*}, each closed with }.")
		}
	}

	// Map order is random; sort so the reason is stable between reads.
	slices.Sort(headerProblems)
	problems = append(problems, headerProblems...)

	if remote.Timeout != nil && (remote.Timeout.Duration < minRemoteTimeout || remote.Timeout.Duration > maxRemoteTimeout) {
		problems = append(problems, "`remote.timeout` must be 1s to 5m.")
	}

	return problems
}

// remoteURLProblem returns why rawURL can't be a remote server's URL, or ""
// when it is https://, or http:// to a cluster-internal Service host.
func remoteURLProblem(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" {
		return "`remote.url` must be an absolute http:// or https:// URL with a host."
	}

	// Credentials belong in headers, where secrets fill them per user, and
	// not in the URL, which the status and logs may show.
	if parsed.User != nil {
		return "`remote.url` must not contain a user name or password; send credentials in `remote.headers`."
	}

	switch parsed.Scheme {
	case "https":
		return ""
	case "http":
		host := strings.ToLower(parsed.Hostname())
		for _, suffix := range clusterServiceSuffixes {
			if before, found := strings.CutSuffix(host, suffix); found && before != "" && !strings.HasSuffix(before, ".") {
				return ""
			}
		}

		return "`remote.url` may use http:// only for a cluster-internal host ending in .svc or .svc.cluster.local; use https:// for any other host."
	default:
		return "`remote.url` must start with https:// or http://."
	}
}

// headerValueAllowed reports whether every variable in value is
// ${secret:NAME} or ${principal.*} and every ${ is closed.
func headerValueAllowed(value string) bool {
	for index := 0; index < len(value); {
		rest := value[index:]

		switch {
		case strings.HasPrefix(rest, "$${"):
			index += 3
		case strings.HasPrefix(rest, "${"):
			end := strings.IndexByte(rest[2:], '}')
			if end < 0 {
				return false
			}

			variable := rest[2 : 2+end]
			secret, isSecret := strings.CutPrefix(variable, "secret:")
			field, isPrincipal := strings.CutPrefix(variable, "principal.")

			if !(isSecret && secret != "") && !(isPrincipal && field != "") {
				return false
			}

			index += 2 + end + 1
		default:
			index++
		}
	}

	return true
}
