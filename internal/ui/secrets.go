package ui

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/secrets"
)

// SecretStore is the secret broker the Secrets page uses, the same one
// behind `/api/v1/secrets`. *secrets.Broker implements it.
type SecretStore interface {
	Set(ctx context.Context, principal, name string, value []byte) (secrets.Info, error)
	List(ctx context.Context, principal string) ([]secrets.Info, error)
	Delete(ctx context.Context, principal, name string) error
}

// SecretsPath is the Secrets page.
const SecretsPath = "/ui/secrets"

type secretsPage struct {
	Secrets []secrets.Info
}

// secretsPage lists the user's secret names and dates. It never reads a
// value.
func (u *UI) secretsPage(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())

	infos, err := u.options.Secrets.List(request.Context(), session.Principal)
	if err != nil {
		u.logger.ErrorContext(request.Context(), "cannot list secrets for the web UI", "error", err)
		http.Error(writer, "The gateway can't read your secrets. Try again later.", http.StatusInternalServerError)

		return
	}

	u.render(writer, request, "secrets", "Secrets", secretsPage{Secrets: infos})
}

// setSecret stores the form's value as the user's secret and redirects to
// the page. A textarea value has CRLF converted to LF; a file's bytes are
// kept exactly.
func (u *UI) setSecret(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())

	value, status, message := secretFormValue(request)
	if status != 0 {
		http.Error(writer, message, status)

		return
	}

	name := request.PostFormValue("name")

	_, err := u.options.Secrets.Set(request.Context(), session.Principal, name, value)
	switch {
	case errors.Is(err, secrets.ErrInvalidName):
		http.Error(writer, "The secret name is invalid. It must match ^[A-Za-z0-9._-]{1,128}$.", http.StatusBadRequest)

		return
	case errors.Is(err, secrets.ErrValueTooLong):
		http.Error(writer, secrets.ErrValueTooLong.Error()+".", http.StatusRequestEntityTooLarge)

		return
	case err != nil:
		u.secretsFailed(writer, request, "storing a secret failed", err)

		return
	}

	if !u.recordSecretEvent(writer, request, audit.TypeSecretSet, session, name) {
		return
	}

	http.Redirect(writer, request, SecretsPath, http.StatusSeeOther)
}

// deleteSecret deletes the user's secret and redirects to the page, also
// when the secret was already gone.
func (u *UI) deleteSecret(writer http.ResponseWriter, request *http.Request) {
	session, _ := SessionFrom(request.Context())
	name := request.PathValue("name")

	err := u.options.Secrets.Delete(request.Context(), session.Principal, name)

	switch {
	case errors.Is(err, secrets.ErrNotFound):
		http.Redirect(writer, request, SecretsPath, http.StatusSeeOther)

		return
	case err != nil:
		u.secretsFailed(writer, request, "deleting a secret failed", err)

		return
	}

	if !u.recordSecretEvent(writer, request, audit.TypeSecretDelete, session, name) {
		return
	}

	http.Redirect(writer, request, SecretsPath, http.StatusSeeOther)
}

// secretFormValue reads the value from the form's file or textarea. A
// non-zero status is the refusal to answer with.
func secretFormValue(request *http.Request) (value []byte, status int, message string) {
	file, header, err := request.FormFile("file")
	hasFile := err == nil

	if hasFile {
		defer func() { _ = file.Close() }()
	}

	text := request.PostFormValue("value")

	switch {
	case hasFile && header.Filename != "" && text != "":
		return nil, http.StatusBadRequest, "Fill in the value or choose a file, not both."
	case hasFile && header.Filename != "":
		data, err := io.ReadAll(io.LimitReader(file, secrets.MaxValueBytes+1))
		if err != nil {
			return nil, http.StatusBadRequest, "The gateway can't read the file."
		}

		return data, 0, ""
	case text == "":
		return nil, http.StatusBadRequest, "Fill in the value or choose a file."
	}

	return []byte(strings.ReplaceAll(text, "\r\n", "\n")), 0, ""
}

// recordSecretEvent writes the audit event for a change made in the UI:
// `user` attestation and no workspace or session. When that fails it
// answers 500 and returns false.
func (u *UI) recordSecretEvent(writer http.ResponseWriter, request *http.Request, eventType string, session Session, name string) bool {
	err := u.options.Auditor.Record(request.Context(), audit.Event{
		Type:        eventType,
		Principal:   session.Principal,
		Attestation: auth.AttestationUser,
		Details:     map[string]any{"secret": name},
	})
	if err != nil {
		u.secretsFailed(writer, request, "writing the audit event failed", err)

		return false
	}

	return true
}

// secretsFailed logs err, which never holds a secret value, and answers 500.
func (u *UI) secretsFailed(writer http.ResponseWriter, request *http.Request, message string, err error) {
	u.logger.ErrorContext(request.Context(), message, "error", err)
	http.Error(writer, "The gateway could not complete the request. Try again later.", http.StatusInternalServerError)
}
