// Package principal decides how a person, workload or service account is
// shown to people. Raw OIDC subjects are often long opaque hashes, so every
// surface shows the name or email first and keeps the raw ID for hover and
// copy.
package principal

import (
	"fmt"
	"strings"
)

// Prefixes of the principals that are not people.
const (
	serviceAccountPrefix = "system:serviceaccount:"
	spiffePrefix         = "spiffe://"
)

// shortKeep is how many runes of an opaque ID stay visible when it is
// shortened. maxSPIFFE is the longest SPIFFE ID shown in full.
const (
	shortKeep = 8
	maxSPIFFE = 48
)

// Profile holds the newest display fields toolgate has seen for a principal.
// Email is only ever a value the IdP verified.
type Profile struct {
	Email    string
	Name     string
	Username string
}

// Display is how one principal is shown.
type Display struct {
	// Text is the main text.
	Text string
	// Hover is the title text shown on hover. It holds the raw ID, and for
	// the signed-in user's own principal the name or email as well.
	Hover string
	// Raw is the principal exactly as stored.
	Raw string
	// Copyable is true when the main text is a shortened opaque ID, so the
	// page offers a copy button for Raw.
	Copyable bool
}

// Of returns how to show principal. profile is nil when toolgate has never
// seen a name for it. self is the signed-in user's own principal.
func Of(principal string, profile *Profile, self string) Display {
	display := Display{Raw: principal, Hover: principal}

	switch {
	case principal == "":
		display.Text = ""
		display.Hover = ""
	case strings.HasPrefix(principal, serviceAccountPrefix):
		display.Text = "serviceaccount " + strings.Replace(strings.TrimPrefix(principal, serviceAccountPrefix), ":", "/", 1)
	case strings.HasPrefix(principal, spiffePrefix):
		display.Text = shortSPIFFE(principal)
	default:
		name := ""
		if profile != nil {
			name = Person(*profile)
		}

		if name == "" {
			display.Text = "user " + Short(principal)
			display.Copyable = true
		} else {
			display.Text = name
		}

		if principal == self {
			display.Hover = fmt.Sprintf("%s (you)\n%s", name, principal)
			if name == "" {
				display.Hover = principal
			}

			display.Text = "you"
			display.Copyable = false
		}
	}

	return display
}

// Person formats a user's profile: `Name <email>`, the email alone, the
// name alone or the username. It returns "" when the profile has none.
func Person(profile Profile) string {
	switch {
	case profile.Name != "" && profile.Email != "":
		return profile.Name + " <" + profile.Email + ">"
	case profile.Email != "":
		return profile.Email
	case profile.Name != "":
		return profile.Name
	default:
		return profile.Username
	}
}

// Short keeps the start of a long opaque ID.
func Short(id string) string {
	runes := []rune(id)
	if len(runes) <= shortKeep+1 {
		return id
	}

	return string(runes[:shortKeep]) + "…"
}

func shortSPIFFE(id string) string {
	runes := []rune(id)
	if len(runes) <= maxSPIFFE {
		return id
	}

	return string(runes[:maxSPIFFE-1]) + "…"
}

// Workspace returns a short label for a workspace ID, or "" when the ID has
// no known form and should be shown as is.
func Workspace(workspace string) string {
	parts := strings.Split(workspace, "/")

	switch {
	case parts[0] == "claimed" && len(parts) >= 2:
		return "unverified: " + strings.Join(parts[1:], "/")
	case parts[0] == "coder" && len(parts) == 3:
		return "Coder workspace " + parts[2]
	default:
		return ""
	}
}
