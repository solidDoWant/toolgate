package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// accessRequestRow is one access request on the Approvals page.
type accessRequestRow struct {
	ID string
	// Change says what the request would change.
	Change string
	// AskedBy is the client session that asked, or the workspace when the
	// request names no session.
	AskedBy   string
	Status    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// approvalsPage is the data of the Approvals page.
type approvalsPage struct {
	Approvals      []approvalRow
	AccessRequests []accessRequestRow
	// Recent are the last few access requests that were decided or expired.
	Recent []accessRequestRow
}

func newAccessRequestRow(request db.AccessRequest, clientSession string, now time.Time) accessRequestRow {
	status := request.Status
	if status == "pending" && !request.ExpiresAt.Time.After(now) {
		status = "expired"
	}

	asked := clientSession
	if asked == "" {
		asked = request.Workspace
	}

	return accessRequestRow{
		ID:        request.ID,
		Change:    describeAccessRequest(request),
		AskedBy:   asked,
		Status:    status,
		CreatedAt: request.CreatedAt.Time,
		ExpiresAt: request.ExpiresAt.Time,
	}
}

// describeAccessRequest summarizes the change a request asks for: the
// action, the servers and tools, and the target workspace or session.
func describeAccessRequest(request db.AccessRequest) string {
	switch request.Kind {
	case "server":
		return fmt.Sprintf("Allow server %s in workspace %s", request.Server.String, request.Workspace)
	case "scope":
		var change gateway.ScopeChange
		if json.Unmarshal(request.Change, &change) != nil {
			return "Allow access"
		}

		items := make([]string, 0, len(change.Items))
		for _, item := range change.Items {
			if item.Tool != "" {
				items = append(items, item.Server+"/"+item.Tool)
			} else {
				items = append(items, item.Server)
			}
		}

		summary := "Allow " + strings.Join(items, ", ")

		switch {
		case change.Defaults != nil:
			summary += "; " + describeDefaults(change.Defaults.Verified, change.Defaults.Unverified)
		case change.Unverified:
			summary += " for unverified sessions"
		case change.Session != "":
			summary += " for session " + change.Session
		default:
			summary += " in workspace " + change.Workspace
		}

		return summary
	case "key":
		var change gateway.KeyChange
		if json.Unmarshal(request.Change, &change) != nil {
			return "Register workspace key"
		}

		return fmt.Sprintf("Register workspace key %s (%s)", change.Name, shortFingerprint(change.KeyID))
	default:
		var change gateway.WorkspaceChange
		if json.Unmarshal(request.Change, &change) != nil {
			return "Change access"
		}

		return describeWorkspaceChange(change)
	}
}

func describeDefaults(verified, unverified string) string {
	var parts []string

	if verified != "" {
		parts = append(parts, "default for verified sessions: "+verified)
	}

	if unverified != "" {
		parts = append(parts, "default for unverified sessions: "+unverified)
	}

	return strings.Join(parts, ", ")
}

func describeWorkspaceChange(change gateway.WorkspaceChange) string {
	servers := strings.Join(change.Servers, ", ")

	target := "workspace " + change.Workspace

	switch {
	case change.UnverifiedList:
		target = "unverified sessions"
	case change.Session != "":
		target = "session " + change.Session
	}

	switch change.Action {
	case gateway.WorkspaceDefaults:
		return "Change defaults: " + describeDefaults(change.Verified, change.Unverified)
	case gateway.WorkspaceAllow:
		return fmt.Sprintf("Allow %s for %s", servers, target)
	case gateway.WorkspaceDeny:
		return fmt.Sprintf("Deny %s for %s", servers, target)
	case gateway.WorkspaceClear:
		return "Clear the list of " + target
	default:
		return "Change access for " + target
	}
}

// accessRequestRows reads the user's pending and recent access requests.
func (u *UI) accessRequestRows(ctx context.Context, principal string) (pending, recent []accessRequestRow, err error) {
	now := time.Now()

	pendingRows, err := u.queries.ListPendingAccessRequestsOfPrincipal(ctx, principal)
	if err != nil {
		return nil, nil, err
	}

	for _, row := range pendingRows {
		pending = append(pending, newAccessRequestRow(row.AccessRequest, row.ClientSessionID.String, now))
	}

	recentRows, err := u.queries.ListRecentDecidedAccessRequestsOfPrincipal(ctx, principal)
	if err != nil {
		return nil, nil, err
	}

	for _, row := range recentRows {
		recent = append(recent, newAccessRequestRow(row.AccessRequest, row.ClientSessionID.String, now))
	}

	return pending, recent, nil
}
