//go:build integration

package ui_test

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/secrets"
	toolgatetest "github.com/solidDoWant/toolgate/internal/testutil"
	"github.com/solidDoWant/toolgate/internal/ui"
)

// secretField is one part of the secret form: a plain field, or a file when
// filename is set.
type secretField struct {
	name, value, filename string
}

// multipartBody encodes fields as a browser would.
func multipartBody(t *testing.T, fields []secretField) (body []byte, contentType string) {
	t.Helper()

	var buffer bytes.Buffer

	writer := multipart.NewWriter(&buffer)

	for _, field := range fields {
		if field.filename == "" {
			require.NoError(t, writer.WriteField(field.name, field.value))

			continue
		}

		part, err := writer.CreateFormFile(field.name, field.filename)
		require.NoError(t, err)
		_, err = part.Write([]byte(field.value))
		require.NoError(t, err)
	}

	require.NoError(t, writer.Close())

	return buffer.Bytes(), writer.FormDataContentType()
}

func postRaw(t *testing.T, client *http.Client, target string, body []byte, contentType string) *http.Response {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", contentType)

	response, err := client.Do(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })

	return response
}

// setSecretForm posts the secret form with the session's CSRF token.
func (e *uiEnv) setSecretForm(t *testing.T, browser *http.Client, cookie string, fields ...secretField) *http.Response {
	t.Helper()

	fields = append([]secretField{{name: ui.CSRFField, value: csrfTokenOf(cookie)}}, fields...)
	body, contentType := multipartBody(t, fields)

	return postRaw(t, browser, e.server.URL+"/ui/secrets", body, contentType)
}

func (e *uiEnv) storedSecret(t *testing.T, principal, name string) (string, bool) {
	t.Helper()

	var ciphertext []byte

	err := e.pool.QueryRow(t.Context(), "SELECT ciphertext FROM user_secrets WHERE principal = $1 AND name = $2", principal, name).Scan(&ciphertext)
	if err != nil {
		return "", false
	}

	return string(ciphertext), true
}

// decrypted reads the value through a broker over the same database.
func (e *uiEnv) secretValue(t *testing.T, principal, name string) string {
	t.Helper()

	value, _, err := e.broker.Get(t.Context(), principal, name)
	require.NoError(t, err)

	return string(value)
}

func TestSecretsPageSetsListsAndDeletes(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "secrets-session", nil)
	token := csrfTokenOf("secrets-session")

	const knownValue = "hunter2-known-value"

	response := env.setSecretForm(t, browser, "secrets-session", secretField{name: "name", value: "api-key"}, secretField{name: "value", value: knownValue})
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, "/ui/secrets", location(t, response).Path)
	assert.Equal(t, knownValue, env.secretValue(t, "dave", "api-key"))

	page := readBody(t, get(t, browser, env.server.URL+"/ui/secrets"))
	assert.Contains(t, page, `class="font-mono">api-key</td>`)
	assert.NotContains(t, page, knownValue)
	assert.Contains(t, page, `enctype="multipart/form-data"`)
	assert.Contains(t, page, `name="value"`)
	assert.Contains(t, page, `type="file"`)
	assert.Contains(t, page, `action="/ui/secrets/api-key/delete"`)
	assert.Contains(t, page, `Delete secret api-key? Servers that use it stop working until you set it again.`)
	assert.Contains(t, page, `<a href="/ui/servers/github">github</a>`)
	assert.Contains(t, page, token)

	// Replace.
	response = env.setSecretForm(t, browser, "secrets-session", secretField{name: "name", value: "api-key"}, secretField{name: "value", value: "second"})
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, "second", env.secretValue(t, "dave", "api-key"))
	assert.Equal(t, 1, env.countRows(t, "user_secrets"))

	// Delete.
	response = post(t, browser, env.server.URL+"/ui/secrets/api-key/delete", map[string][]string{ui.CSRFField: {token}})
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, 0, env.countRows(t, "user_secrets"))

	// Deleting a secret that is gone also redirects.
	response = post(t, browser, env.server.URL+"/ui/secrets/api-key/delete", map[string][]string{ui.CSRFField: {token}})
	assert.Equal(t, http.StatusSeeOther, response.StatusCode)

	assert.NotContains(t, readBody(t, get(t, browser, env.server.URL+"/ui/secrets")), "api-key")

	rows, err := env.pool.Query(t.Context(), `SELECT type, principal, workspace, session, attestation, details->>'secret' FROM audit_events WHERE type LIKE 'secret.%' ORDER BY id`)
	require.NoError(t, err)

	defer rows.Close()

	type event struct{ kind, principal, workspace, session, attestation, secret string }

	var events []event

	for rows.Next() {
		var item event
		require.NoError(t, rows.Scan(&item.kind, &item.principal, &item.workspace, &item.session, &item.attestation, &item.secret))
		events = append(events, item)
	}

	require.NoError(t, rows.Err())
	assert.Equal(t, []event{
		{"secret.set", "dave", "", "", "user", "api-key"},
		{"secret.set", "dave", "", "", "user", "api-key"},
		{"secret.delete", "dave", "", "", "user", "api-key"},
	}, events, "one event per change, none for the delete of a missing secret")
}

func TestSecretsPageOnlyActsOnTheSessionsPrincipal(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "own-session", nil)

	_, err := env.broker.Set(t.Context(), "erin", "theirs", []byte("erin-value"))
	require.NoError(t, err)

	assert.NotContains(t, readBody(t, get(t, browser, env.server.URL+"/ui/secrets")), "theirs")

	response := post(t, browser, env.server.URL+"/ui/secrets/theirs/delete", map[string][]string{ui.CSRFField: {csrfTokenOf("own-session")}})
	assert.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, "erin-value", env.secretValue(t, "erin", "theirs"))
}

func TestSecretsRoutesRefuseUnauthenticatedRequests(t *testing.T) {
	env := startUIEnv(t, "")
	anonymous := env.browser(t)

	response := get(t, anonymous, env.server.URL+"/ui/secrets")
	assert.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, ui.LoginPath, location(t, response).Path)

	for _, target := range []string{"/ui/secrets", "/ui/secrets/x/delete"} {
		response = post(t, anonymous, env.server.URL+target, map[string][]string{"name": {"x"}, "value": {"v"}})
		assert.Equal(t, http.StatusUnauthorized, response.StatusCode, target)
	}

	assert.Equal(t, 0, env.countRows(t, "user_secrets"))
}

func TestSecretFormWithoutCSRFTokenChangesNothing(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "csrf-secret", nil)

	body, contentType := multipartBody(t, []secretField{{name: ui.CSRFField, value: "forged"}, {name: "name", value: "x"}, {name: "value", value: "v"}})
	response := postRaw(t, browser, env.server.URL+"/ui/secrets", body, contentType)
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.Equal(t, 0, env.countRows(t, "user_secrets"))
}

func TestSecretValueNewlinesAndFiles(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "newline-session", nil)

	// A browser submits a textarea with CRLF line breaks.
	response := env.setSecretForm(t, browser, "newline-session", secretField{name: "name", value: "pem"}, secretField{name: "value", value: "a\r\nb\r\n\r\nc\rd\n"})
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, "a\nb\n\nc\rd\n", env.secretValue(t, "dave", "pem"), "CRLF becomes LF and nothing else changes")

	fileBytes := "a\r\nb\r\n\x00\xff"
	response = env.setSecretForm(t, browser, "newline-session", secretField{name: "name", value: "binary"}, secretField{name: "file", value: fileBytes, filename: "key.bin"})
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, fileBytes, env.secretValue(t, "dave", "binary"), "a file's bytes are kept exactly")

	// An empty file field (no file chosen) next to a textarea value.
	response = env.setSecretForm(t, browser, "newline-session", secretField{name: "name", value: "both"},
		secretField{name: "value", value: "typed"}, secretField{name: "file", value: "", filename: ""})
	require.Equal(t, http.StatusSeeOther, response.StatusCode)
	assert.Equal(t, "typed", env.secretValue(t, "dave", "both"))

	// A value and a file together are refused.
	response = env.setSecretForm(t, browser, "newline-session", secretField{name: "name", value: "clash"},
		secretField{name: "value", value: "typed"}, secretField{name: "file", value: "filed", filename: "f"})
	assert.Equal(t, http.StatusBadRequest, response.StatusCode)

	// Neither is refused too.
	response = env.setSecretForm(t, browser, "newline-session", secretField{name: "name", value: "empty"})
	assert.Equal(t, http.StatusBadRequest, response.StatusCode)

	// A bad name is refused.
	response = env.setSecretForm(t, browser, "newline-session", secretField{name: "name", value: "bad name"}, secretField{name: "value", value: "v"})
	assert.Equal(t, http.StatusBadRequest, response.StatusCode)

	_, found := env.storedSecret(t, "dave", "clash")
	assert.False(t, found)
}

func TestSecretValueLimitIsFourMiB(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "limit-secret", nil)

	tests := []struct {
		name       string
		field      secretField
		wantStatus int
	}{
		{name: "file of exactly 4 MiB", field: secretField{name: "file", value: strings.Repeat("a", secrets.MaxValueBytes), filename: "f"}, wantStatus: http.StatusSeeOther},
		{name: "file of 4 MiB and one byte", field: secretField{name: "file", value: strings.Repeat("a", secrets.MaxValueBytes+1), filename: "f"}, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "textarea of exactly 4 MiB", field: secretField{name: "value", value: strings.Repeat("a", secrets.MaxValueBytes)}, wantStatus: http.StatusSeeOther},
		{name: "textarea of 4 MiB and one byte", field: secretField{name: "value", value: strings.Repeat("a", secrets.MaxValueBytes+1)}, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "textarea of 4 MiB after CRLF conversion", field: secretField{name: "value", value: strings.Repeat("\r\n", secrets.MaxValueBytes/2)}, wantStatus: http.StatusSeeOther},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := env.setSecretForm(t, browser, "limit-secret", secretField{name: "name", value: "big"}, test.field)
			assert.Equal(t, test.wantStatus, response.StatusCode)
			_, _ = io.Copy(io.Discard, response.Body)
		})
	}
}

func TestSecretFormBodyLimitIsFourMiBPlus64KiB(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "body-limit", nil)

	boundary := "xxxx"
	build := func(size int) []byte {
		prefix := "--" + boundary + "\r\nContent-Disposition: form-data; name=\"pad\"\r\n\r\n"
		suffix := "\r\n--" + boundary + "--\r\n"

		return []byte(prefix + strings.Repeat("a", size-len(prefix)-len(suffix)) + suffix)
	}

	contentType := "multipart/form-data; boundary=" + boundary

	response := postRaw(t, browser, env.server.URL+"/ui/secrets", build(ui.MaxSecretFormBytes), contentType)
	assert.Equal(t, http.StatusForbidden, response.StatusCode, "at the limit the body is read and then the missing token refuses it")

	response = postRaw(t, browser, env.server.URL+"/ui/secrets", build(ui.MaxSecretFormBytes+1), contentType)
	assert.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)

	response = postRaw(t, browser, env.server.URL+"/ui/secrets/x/delete", build(ui.MaxFormBytes+1), contentType)
	assert.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode, "other forms keep the small limit")
}

// In a browser, a user sets, replaces and deletes a secret, and
// after saving the value appears in no page.
func TestSecretsInBrowser(t *testing.T) {
	env := startUIEnv(t, "")
	env.insertSession(t, "browser-secrets", nil)

	browser := toolgatetest.StartBrowser(t, chromedp.Flag("ignore-certificate-errors", true))

	ctx, cancel := context.WithTimeout(browser, 2*time.Minute)
	defer cancel()

	secure := true
	_, err := chromedp.Call(ctx, network.SetCookie, network.SetCookieParams{
		Name: ui.SessionCookie, Value: "browser-secrets", URL: env.server.URL + "/", Path: "/", Secure: &secure, HTTPOnly: &secure,
	})
	require.NoError(t, err)

	const (
		first  = "first-known-value-7f3a"
		second = "second-known-value-91bc"
	)

	save := func(value string) {
		require.NoError(t, chromedp.Do(ctx,
			toolgatetest.Navigate(env.server.URL+"/ui/secrets"),
			chromedp.Click(`main a[data-dialog="set-secret-dialog"]`),
			chromedp.WaitVisible(`#set-secret button`),
			chromedp.SetValue(`#set-secret input[name="name"]`, "browser-key"),
			chromedp.SetValue(`#set-secret textarea[name="value"]`, value),
			chromedp.Click(`#set-secret button`),
		))
		require.Eventually(t, func() bool {
			stored, _, err := env.broker.Get(t.Context(), "dave", "browser-key")

			return err == nil && string(stored) == value
		}, time.Minute, 100*time.Millisecond)
	}

	pageText := func() string {
		require.NoError(t, chromedp.Do(ctx, toolgatetest.Navigate(env.server.URL+"/ui/secrets"), chromedp.WaitVisible(`main a[data-dialog="set-secret-dialog"]`)))

		text, err := chromedp.Run(ctx, chromedp.OuterHTML("html"))
		require.NoError(t, err)

		return text
	}

	save(first)
	assert.Equal(t, first, env.secretValue(t, "dave", "browser-key"))
	assert.NotContains(t, pageText(), first, "the page after saving doesn't hold the value")

	require.NoError(t, chromedp.Do(ctx, toolgatetest.Navigate(env.server.URL+"/ui/secrets"), chromedp.WaitVisible(`main a[data-dialog="set-secret-dialog"]`)))
	assert.NotContains(t, pageText(), first)

	save(second)
	assert.Equal(t, second, env.secretValue(t, "dave", "browser-key"))
	assert.NotContains(t, pageText(), second)
	assert.Equal(t, 1, env.countRows(t, "user_secrets"))

	require.NoError(t, chromedp.Do(ctx, toolgatetest.Navigate(env.server.URL+"/ui/secrets"), chromedp.WaitVisible(`#secrets summary`)))

	// Opening the row action shows the question and deletes nothing.
	require.NoError(t, chromedp.Do(ctx, chromedp.Click(`#secrets summary`), chromedp.WaitVisible(`#secrets form button`)))

	question, err := chromedp.Run(ctx, chromedp.Text(`#secrets form p`))
	require.NoError(t, err)
	assert.Equal(t, "Delete secret browser-key? Servers that use it stop working until you set it again.", question)
	assert.Equal(t, 1, env.countRows(t, "user_secrets"))

	require.NoError(t, chromedp.Do(ctx, chromedp.Click(`#secrets form button`)))
	require.Eventually(t, func() bool { return pageShows(ctx, "You have no secrets.") }, time.Minute, 100*time.Millisecond)
	assert.Equal(t, 0, env.countRows(t, "user_secrets"))
}

// Without JavaScript the Set a secret link goes to a page with the form, and
// only a signed-in user may open it.
func TestSecretFormPageShowsTheFormAndRefusesAnonymousUsers(t *testing.T) {
	env := startUIEnv(t, "")
	browser := env.insertSession(t, "secret-form-session", nil)

	page := readBody(t, get(t, browser, env.server.URL+"/ui/secrets"))
	assert.Contains(t, page, `href="/ui/secrets/new"`)

	response := get(t, browser, env.server.URL+"/ui/secrets/new")
	require.Equal(t, http.StatusOK, response.StatusCode)
	assert.Contains(t, readBody(t, response), `action="/ui/secrets"`)

	anonymous := get(t, env.browser(t), env.server.URL+"/ui/secrets/new")
	assert.Equal(t, http.StatusSeeOther, anonymous.StatusCode)
	assert.Equal(t, ui.LoginPath, location(t, anonymous).Path)
}
