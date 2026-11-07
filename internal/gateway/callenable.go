package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/policy"
)

// Refusal reasons of a call that enables its server.
const (
	reasonMissingSecret = "missing-secret"
	reasonLimit         = "limit"
	reasonFirstCall     = "first-call"
	reasonRemote        = "remote-unsupported"
)

// remoteUnsupported is the refusal of enabling a remote template. This
// gateway accepts remote templates in its catalog but can't connect to
// them yet, and they have nothing to launch, so enabling one must start
// nothing.
func remoteUnsupported(server string) error {
	return fmt.Errorf("Server `%s` is a remote server, and this gateway version can't connect to remote servers yet.", server) //nolint:staticcheck // A sentence for the model.
}

// enableForCall is the policy and enable part of a call of a
// server the session hasn't enabled: it decides policy for the tool, asks
// for a session addition when only the workspace-scope step refuses, and
// enables the server. A non-nil result or error ends the call; both nil
// means the server is now enabled. Every refusal writes tool.decision.
func (m *metaTools) enableForCall(ctx context.Context, request *mcp.CallToolRequest, current requestContext, template *v1alpha1.MCPServerTemplate, tool string) (*mcp.CallToolResult, error) {
	server := template.Name

	state, err := m.policyState(ctx, current.identity)
	if err != nil {
		return nil, err
	}

	decision := m.decide(ctx, m.policyRequest(current, template, tool, state))

	requestID := ""

	if m.asksForAddition(current, decision) {
		id, result, err := m.requestSessionAddition(ctx, additionEnable{current: current, request: request, template: template, tool: tool})

		var refusal *additionRefusal
		if errors.As(err, &refusal) {
			return errorResult(refusal.message), nil
		}

		if result != nil || err != nil {
			return result, err
		}

		requestID, decision = id, policy.Decision{Allowed: true}
	}

	if !decision.Allowed {
		return m.refuseCall(ctx, current, server, tool, refusalDetails(decision), decision.Reason)
	}

	if template.Spec.Remote != nil {
		return m.refuseCall(ctx, current, server, tool, map[string]any{"reason": reasonRemote}, remoteUnsupported(server).Error())
	}

	if err := m.checkSecrets(ctx, current, template); err != nil {
		return m.refuseCall(ctx, current, server, tool, map[string]any{"reason": reasonMissingSecret}, err.Error())
	}

	lazy, err := m.startsOnCall(ctx, template)
	if err != nil {
		return nil, err
	}

	_, err = m.enableInTransaction(ctx, current, template, enableOptions{requestID: requestID, reason: reasonFirstCall, lazy: lazy})

	var limitErr *instanceLimitError
	if errors.As(err, &limitErr) {
		return m.refuseCall(ctx, current, server, tool, map[string]any{"reason": reasonLimit, "limit": limitErr.limit}, limitErr.Error())
	}

	if errors.Is(err, errTooManyServers) {
		return m.refuseCall(ctx, current, server, tool, map[string]any{"reason": reasonLimit, "limit": MaxEnabledServers}, err.Error())
	}

	if err != nil {
		return nil, err
	}

	// The server is enabled and committed: tell the client its tools may
	// have changed.
	if _, err := m.syncSession(ctx, request.Session, current); err != nil {
		return nil, fmt.Errorf("update the session's tools: %w", err)
	}

	return nil, nil
}
