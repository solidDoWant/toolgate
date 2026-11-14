package ui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/solidDoWant/toolgate/internal/principal"
)

// Outcomes an audit row can show as a badge.
const (
	outcomeOK       = "ok"
	outcomeRefused  = "refused"
	outcomeError    = "error"
	outcomePending  = "pending"
	outcomeApproved = "approved"
	outcomeDenied   = "denied"
	outcomeNone     = ""
)

// auditSummary is the one-line account of an event: Prefix, then Subject in
// code style, then Suffix.
type auditSummary struct {
	Prefix  string
	Subject string
	Suffix  string
	Outcome string
}

// Text is the summary as plain text.
func (s auditSummary) Text() string {
	if s.Subject == "" {
		return s.Prefix + s.Suffix
	}

	return s.Prefix + "`" + s.Subject + "`" + s.Suffix
}

// auditCategories groups event types for the category filter. A type that
// is in no group shows under "other" and is found by no category.
var auditCategories = []struct {
	Key, Label string
	Types      []string
}{
	{"calls", "Tool calls", []string{"tool.decision", "tool.result"}},
	{"access", "Access and scope changes", []string{
		"access.request", "access.approve", "access.deny", "access.expire", "approval.request", "approval.approve",
		"approval.deny", "approval.expire", "grant.create", "grant.revoke", "block.create", "rule.change",
		"workspace.change", "workspace.defaults", "session.addition",
		"workspace.key.register", "workspace.key.approve", "workspace.key.replace", "workspace.key.revoke",
	}},
	{"signins", "Sign-ins", []string{"session.start", "session.resume", "session.end", "session.forget", "auth.refuse"}},
	{"servers", "Server lifecycle", []string{
		"server.enable", "server.disable", "instance.start", "instance.ready", "instance.fail", "instance.stop",
		"instance.health", "instance.reapply", "secret.set", "secret.delete",
		"volume.create", "volume.lost", "volume.delete", "credential.exchange",
	}},
}

func categoryTypes(key string) []string {
	for _, category := range auditCategories {
		if category.Key == key {
			return category.Types
		}
	}

	return nil
}

// auditDetails reads the details object of an event; a malformed one reads
// as empty.
func auditDetails(raw []byte) map[string]any {
	details := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &details)
	}

	return details
}

func detailString(details map[string]any, key string) string {
	switch value := details[key].(type) {
	case string:
		return value
	case float64:
		return fmt.Sprintf("%.0f", value)
	default:
		return ""
	}
}

// refusalWhy turns the details of a refusal into a short reason.
func refusalWhy(details map[string]any) string {
	switch {
	case detailString(details, "rule") != "":
		return "blocked by rule " + detailString(details, "rule")
	case detailString(details, "block") != "":
		return "blocked by block " + detailString(details, "block")
	case detailString(details, "reason") != "":
		return strings.ReplaceAll(detailString(details, "reason"), "_", " ")
	default:
		return "no reason recorded"
	}
}

// summarize describes event in one line. Every type the gateway writes has
// a case; any other type gets the generic line.
func summarize(event auditEvent) auditSummary {
	details := auditDetails([]byte(event.Details))

	target := event.Server
	if event.Tool != "" {
		target = event.Server + "__" + event.Tool
	}

	switch event.Type {
	case "tool.decision":
		if detailString(details, "decision") == "refuse" {
			return auditSummary{"Refused ", target, ": " + refusalWhy(details), outcomeRefused}
		}

		suffix := ": allowed"
		if grant := detailString(details, "grant"); grant != "" {
			suffix += " by grant " + grant
		}

		return auditSummary{"Call of ", target, suffix, outcomeOK}
	case "tool.result":
		outcome := detailString(details, "outcome")
		suffix := ": " + outcome

		if duration := detailString(details, "duration_ms"); duration != "" {
			suffix += ", " + duration + " ms"
		}

		if outcome == "ok" {
			return auditSummary{"Called ", target, suffix, outcomeOK}
		}

		return auditSummary{"Called ", target, suffix, outcomeError}
	case "approval.request":
		return auditSummary{"Approval requested: ", target, "", outcomePending}
	case "approval.approve":
		return auditSummary{"Approved a call of ", target, "", outcomeApproved}
	case "approval.deny":
		return auditSummary{"Denied a call of ", target, "", outcomeDenied}
	case "approval.expire":
		return auditSummary{"Approval expired: ", target, "", outcomeDenied}
	case "server.enable":
		if detailString(details, "decision") == "refuse" {
			return auditSummary{"Refused enabling server ", event.Server, ": " + refusalWhy(details), outcomeRefused}
		}

		return auditSummary{"Enabled server ", event.Server, "", outcomeOK}
	case "server.disable":
		return auditSummary{"Disabled server ", event.Server, "", outcomeOK}
	case "session.start":
		return auditSummary{"Signed in (" + attestationText(event) + ")", "", "", outcomeOK}
	case "session.resume":
		return auditSummary{"Session resumed", "", "", outcomeOK}
	case "session.end":
		suffix := ""
		if reason := detailString(details, "reason"); reason != "" {
			suffix = " (" + strings.ReplaceAll(reason, "_", " ") + ")"
		}

		return auditSummary{"Session ended" + suffix, "", "", outcomeNone}
	case "session.forget":
		return auditSummary{"Session forgotten", "", "", outcomeNone}
	case "auth.refuse":
		return auditSummary{"Sign-in refused: " + refusalWhy(details), "", "", outcomeRefused}
	case "session.addition":
		return auditSummary{"Allowed ", event.Server, " for unverified sessions (approved)", outcomeApproved}
	case "access.request":
		return auditSummary{"Access requested", "", kindSuffix(details), outcomePending}
	case "access.approve":
		return auditSummary{"Access approved", "", kindSuffix(details), outcomeApproved}
	case "access.deny":
		return auditSummary{"Access denied", "", kindSuffix(details), outcomeDenied}
	case "access.expire":
		return auditSummary{"Access request expired", "", kindSuffix(details), outcomeDenied}
	case "workspace.change":
		return auditSummary{"Changed the servers a workspace may use", "", "", outcomeOK}
	case "workspace.key.register":
		return auditSummary{"Asked to register workspace key ", detailString(details, "name"), "", outcomePending}
	case "workspace.key.approve":
		return auditSummary{"Registered workspace key ", detailString(details, "name"), "", outcomeApproved}
	case "workspace.key.replace":
		return auditSummary{"Replaced workspace key ", detailString(details, "name"), "", outcomeOK}
	case "workspace.key.revoke":
		return auditSummary{"Revoked workspace key ", detailString(details, "name"), "", outcomeOK}
	case "workspace.defaults":
		return auditSummary{"Changed the servers allowed for unverified sessions", "", "", outcomeOK}
	case "grant.create":
		return auditSummary{"Grant created", "", "", outcomeOK}
	case "grant.revoke":
		return auditSummary{"Grant revoked", "", "", outcomeOK}
	case "block.create":
		return auditSummary{"Block created", "", "", outcomeOK}
	case "rule.change":
		return auditSummary{"Rules changed", "", "", outcomeNone}
	case "instance.start":
		return auditSummary{"Starting instance of ", event.Server, "", outcomePending}
	case "instance.ready":
		return auditSummary{"Instance of ", event.Server, " is ready", outcomeOK}
	case "instance.fail":
		return auditSummary{"Instance of ", event.Server, " failed", outcomeError}
	case "instance.stop":
		return auditSummary{"Stopped instance of ", event.Server, "", outcomeNone}
	case "instance.health":
		return auditSummary{"Health of ", event.Server, " changed", outcomeNone}
	case "instance.reapply":
		return auditSummary{"Repaired instance of ", event.Server, "", outcomeNone}
	case "secret.set":
		return auditSummary{"Secret set: ", detailString(details, "secret"), "", outcomeOK}
	case "secret.delete":
		return auditSummary{"Secret deleted: ", detailString(details, "secret"), "", outcomeOK}
	case "volume.create":
		return auditSummary{"Volume created: ", detailString(details, "volume"), "", outcomeOK}
	case "volume.lost":
		return auditSummary{"Volume lost: ", detailString(details, "volume"), "", outcomeNone}
	case "volume.delete":
		return auditSummary{"Volume deleted: ", detailString(details, "volume"), "", outcomeOK}
	case "credential.exchange":
		if detailString(details, "outcome") == "failure" {
			return auditSummary{"Credential exchange failed for ", event.Server, " (" + detailString(details, "error") + ")", outcomeError}
		}

		return auditSummary{"Credential exchanged for ", event.Server, "", outcomeOK}
	default:
		return auditSummary{"Event " + event.Type, "", "", outcomeNone}
	}
}

func kindSuffix(details map[string]any) string {
	if kind := detailString(details, "kind"); kind != "" {
		return " (" + strings.ReplaceAll(kind, "_", " ") + ")"
	}

	return ""
}

func attestationText(event auditEvent) string {
	if event.Workspace == "" {
		return event.Attestation
	}

	return event.Attestation + ", " + principal.Workspace(event.Workspace)
}

// knownAuditTypes lists the event types that have a summary.
func knownAuditTypes() []string {
	var types []string

	for _, category := range auditCategories {
		types = append(types, category.Types...)
	}

	return types
}

// auditTypeLabels are the readable names of event types.
var auditTypeLabels = map[string]string{
	"tool.decision": "Tool call decision", "tool.result": "Tool call result",
	"access.request": "Access requested", "access.approve": "Access approved", "access.deny": "Access denied",
	"access.expire": "Access request expired", "approval.request": "Approval requested",
	"approval.approve": "Approval granted", "approval.deny": "Approval denied", "approval.expire": "Approval expired",
	"grant.create": "Grant created", "grant.revoke": "Grant revoked", "block.create": "Block created",
	"rule.change": "Rules changed", "workspace.change": "Workspace servers changed",
	"workspace.key.register": "Workspace key requested", "workspace.key.approve": "Workspace key registered",
	"workspace.key.replace": "Workspace key replaced", "workspace.key.revoke": "Workspace key revoked",
	"workspace.defaults": "Unverified defaults changed", "session.addition": "Server allowed for unverified sessions",
	"session.start": "Session started", "session.resume": "Session resumed", "session.end": "Session ended",
	"session.forget": "Session forgotten", "auth.refuse": "Sign-in refused",
	"server.enable": "Server enabled", "server.disable": "Server disabled", "instance.start": "Instance starting",
	"instance.ready": "Instance ready", "instance.fail": "Instance failed", "instance.stop": "Instance stopped",
	"instance.health": "Instance health changed", "instance.reapply": "Instance repaired",
	"secret.set": "Secret set", "secret.delete": "Secret deleted", "volume.delete": "Volume deleted",
	"volume.create": "Volume created", "volume.lost": "Volume lost",
	"credential.exchange": "Credential exchanged",
}

// auditTypeLabel is the readable name of an event type.
func auditTypeLabel(eventType string) string {
	if label, found := auditTypeLabels[eventType]; found {
		return label
	}

	return eventType
}
