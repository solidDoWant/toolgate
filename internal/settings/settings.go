// Package settings serves the Claude Code settings profiles that match an
// identity, merged into one settings fragment (SPEC.md §13,
// docs/design/phase-1.md §5, "Settings sync").
package settings

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
)

// Path is the route the SessionStart hook fetches.
const Path = "/api/v1/settings"

// NestedKeys are the top-level settings keys merged one level deeper: their
// own keys are merged one by one instead of replaced as a whole.
var NestedKeys = []string{"permissions", "env"}

// Matches reports whether selector applies to identity. The identity must be
// one of the selector's principals or be in one of its groups, and, when the
// selector names an attestation, have that attestation. A selector with no
// principals and no groups applies to no one, like catalog access
// (SPEC.md §8).
func Matches(identity auth.Identity, selector v1alpha1.ProfileSelector) bool {
	if selector.Attestation != "" && selector.Attestation != identity.Attestation {
		return false
	}

	if slices.Contains(selector.Principals, identity.Principal) {
		return true
	}

	return slices.ContainsFunc(identity.Groups, func(group string) bool {
		return slices.Contains(selector.Groups, group)
	})
}

// Merge merges fragments in order, the later one winning for the same key.
// Keys are top-level settings keys, plus one level inside NestedKeys.
func Merge(fragments []map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	merged := map[string]json.RawMessage{}

	for _, fragment := range fragments {
		for key, value := range fragment {
			if !slices.Contains(NestedKeys, key) {
				merged[key] = value

				continue
			}

			var inner map[string]json.RawMessage
			if err := json.Unmarshal(value, &inner); err != nil {
				return nil, fmt.Errorf("%s is not an object: %w", key, err)
			}

			var existing map[string]json.RawMessage
			if previous, ok := merged[key]; ok {
				if err := json.Unmarshal(previous, &existing); err != nil {
					return nil, fmt.Errorf("%s is not an object: %w", key, err)
				}
			}

			if existing == nil {
				existing = map[string]json.RawMessage{}
			}

			for innerKey, innerValue := range inner {
				existing[innerKey] = innerValue
			}

			encoded, err := json.Marshal(existing)
			if err != nil {
				return nil, err
			}

			merged[key] = encoded
		}
	}

	return merged, nil
}

// Handler serves `GET /api/v1/settings`: the merged settings of every
// SettingsProfile in the watched namespaces that matches the request's
// identity, in name order.
type Handler struct {
	// Reader reads SettingsProfile objects, normally the informer cache.
	Reader client.Reader
	// Namespaces are the namespaces whose profiles are read.
	Namespaces []string
	Log        *slog.Logger
}

// Mux is where Register adds the route: an *http.ServeMux or the gateway.
type Mux interface {
	Handle(pattern string, handler http.Handler)
}

// Register adds the route to mux, wrapped by authenticate.
func (h *Handler) Register(mux Mux, authenticate func(http.Handler) http.Handler) {
	mux.Handle("GET "+Path, authenticate(h))
}

// ServeHTTP writes the merged settings as a JSON object.
func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	identity, ok := auth.IdentityFrom(request.Context())
	if !ok {
		http.Error(writer, "Sign in again: run `toolgate login`.", http.StatusUnauthorized)

		return
	}

	merged, err := h.settingsFor(request, identity)
	if err != nil {
		h.logger().ErrorContext(request.Context(), "reading settings profiles failed", "error", err)
		http.Error(writer, "The settings profiles could not be read.", http.StatusInternalServerError)

		return
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(merged)
}

func (h *Handler) settingsFor(request *http.Request, identity auth.Identity) (map[string]json.RawMessage, error) {
	var profiles []v1alpha1.SettingsProfile

	for _, namespace := range h.Namespaces {
		var list v1alpha1.SettingsProfileList
		if err := h.Reader.List(request.Context(), &list, client.InNamespace(namespace)); err != nil {
			return nil, fmt.Errorf("list settings profiles in %s: %w", namespace, err)
		}

		for _, profile := range list.Items {
			if profile.DeletionTimestamp.IsZero() && Matches(identity, profile.Spec.Selector) {
				profiles = append(profiles, profile)
			}
		}
	}

	slices.SortFunc(profiles, func(a, b v1alpha1.SettingsProfile) int {
		if order := strings.Compare(a.Name, b.Name); order != 0 {
			return order
		}

		return strings.Compare(a.Namespace, b.Namespace)
	})

	fragments := make([]map[string]json.RawMessage, 0, len(profiles))

	for _, profile := range profiles {
		var fragment map[string]json.RawMessage

		err := json.Unmarshal(profile.Spec.Settings.Raw, &fragment)
		if err == nil {
			_, err = Merge([]map[string]json.RawMessage{fragment})
		}

		if err != nil {
			// One broken profile must not keep everyone else's settings away.
			h.logger().WarnContext(request.Context(), "settings profile is invalid, skipped",
				"namespace", profile.Namespace, "profile", profile.Name, "reason", err.Error())

			continue
		}

		fragments = append(fragments, fragment)
	}

	merged, err := Merge(fragments)
	if err != nil {
		return nil, err
	}

	return merged, nil
}

func (h *Handler) logger() *slog.Logger {
	if h.Log == nil {
		return slog.Default()
	}

	return h.Log
}
