package ui

import (
	"context"
	"slices"

	"github.com/solidDoWant/toolgate/internal/principal"
)

// principalNames resolves principals to how pages show them. It reads the
// stored profiles once for a page, and shows only principals the page was
// already going to list.
type principalNames struct {
	self     string
	profiles map[string]principal.Profile
}

// namesFor loads the profiles of the principals a page lists. A failure to
// read them is logged and the page falls back to shortened subjects.
func (u *UI) namesFor(ctx context.Context, self string, principals ...string) principalNames {
	names := principalNames{self: self, profiles: map[string]principal.Profile{}}

	wanted := make([]string, 0, len(principals))

	for _, one := range principals {
		if one != "" && !slices.Contains(wanted, one) {
			wanted = append(wanted, one)
		}
	}

	if len(wanted) == 0 {
		return names
	}

	rows, err := u.queries.ListPrincipalProfiles(ctx, wanted)
	if err != nil {
		u.logger.WarnContext(ctx, "cannot read display names for the web UI", "error", err)

		return names
	}

	for _, row := range rows {
		names.profiles[row.Principal] = principal.Profile{Email: row.Email, Name: row.Name, Username: row.Username}
	}

	return names
}

// show returns how to show one principal.
func (n principalNames) show(one string) principal.Display {
	var profile *principal.Profile

	if found, ok := n.profiles[one]; ok {
		profile = &found
	}

	return principal.Of(one, profile, n.self)
}

// workspaceLabel is a workspace ID with the short label pages show for it.
type workspaceLabel struct {
	ID    string
	Label string
}

func labelWorkspace(workspace string) workspaceLabel {
	label := principal.Workspace(workspace)
	if label == "" {
		label = workspace
	}

	return workspaceLabel{ID: workspace, Label: label}
}
