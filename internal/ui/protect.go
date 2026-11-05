package ui

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"mime"
	"net/http"
)

// Response headers and limits of every UI route (docs/design/phase-2.md §5).
const (
	// ContentSecurityPolicy allows only the gateway's own scripts, styles
	// and images, and no framing by any site.
	ContentSecurityPolicy = "default-src 'self'; frame-ancestors 'none'"

	// MaxFormBytes bounds the body of a UI request.
	MaxFormBytes = 64 << 10

	// CSRFField and CSRFHeader carry the session's CSRF token in a form and
	// in an htmx request.
	CSRFField  = "csrf_token"
	CSRFHeader = "X-CSRF-Token"

	// staticCacheControl lets browsers keep a static file for a year. The
	// files carry their version in their names.
	staticCacheControl = "public, max-age=31536000, immutable"
)

// crossOrigin refuses cross-origin unsafe requests by Sec-Fetch-Site and
// Origin.
var crossOrigin = http.NewCrossOriginProtection()

// protect gives a UI handler its response headers, its body limit and
// Go's cross-origin protection.
func protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header := writer.Header()
		header.Set("Content-Security-Policy", ContentSecurityPolicy)
		header.Set("Cache-Control", "no-store")

		if request.Body != nil {
			request.Body = http.MaxBytesReader(writer, request.Body, MaxFormBytes)
		}

		crossOrigin.Handler(next).ServeHTTP(writer, request)
	})
}

// staticFilesHandler serves the embedded static files with a one-year
// cache.
func staticFilesHandler(files http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", staticCacheControl)
		files.ServeHTTP(writer, request)
	})
}

// requireCSRFToken refuses an unsafe request whose CSRF token, from the
// X-CSRF-Token header or the csrf_token form field, isn't the session's.
// It runs after the session is loaded and before the handler, so a refused
// request changes nothing.
func requireCSRFToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(writer, request)

			return
		}

		session, ok := SessionFrom(request.Context())
		if !ok || len(session.CSRFToken) == 0 {
			http.Error(writer, "Your web UI session ended. Sign in again.", http.StatusUnauthorized)

			return
		}

		token := request.Header.Get(CSRFHeader)
		if token == "" {
			if err := parseForm(request); err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					http.Error(writer, "The form is too large.", http.StatusRequestEntityTooLarge)

					return
				}

				http.Error(writer, "The gateway can't read the form.", http.StatusBadRequest)

				return
			}

			token = request.PostFormValue(CSRFField)
		}

		if subtle.ConstantTimeCompare([]byte(token), []byte(session.CSRFTokenValue())) != 1 {
			http.Error(writer, "The form has expired. Reload the page and try again.", http.StatusForbidden)

			return
		}

		next.ServeHTTP(writer, request)
	})
}

// parseForm reads a URL-encoded or multipart body, within the body limit.
func parseForm(request *http.Request) error {
	mediaType, _, _ := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if mediaType == "multipart/form-data" {
		return request.ParseMultipartForm(MaxFormBytes)
	}

	return request.ParseForm()
}

// CSRFTokenValue is the session's CSRF token as forms and htmx send it.
func (s Session) CSRFTokenValue() string {
	return base64.RawURLEncoding.EncodeToString(s.CSRFToken)
}
