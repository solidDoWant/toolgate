package gateway

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/mcprequest"
	"github.com/solidDoWant/toolgate/internal/policy"
)

// Limits of the catalog summary shown to models. Only the admin's template
// description goes in, cut to summaryDescriptionRunes.
const (
	summaryMaxServers       = 20
	summaryDescriptionRunes = 120
	summaryMaxBytes         = 4096
)

// summaryServer is one server the session can use now.
type summaryServer struct {
	Name        string
	Description string
}

// catalogSummary is what a session's models are told about the catalog.
type catalogSummary struct {
	// Usable lists the servers the session can use now.
	Usable []summaryServer
	// Requestable says the user has servers that are blocked but can be
	// requested.
	Requestable bool
}

// summary reads the catalog for the identity. It uses only the policy
// decision and the template, never anything an upstream server reports.
func (m *metaTools) summary(ctx context.Context, identity auth.Identity) (catalogSummary, error) {
	var result catalogSummary

	if m.catalog == nil || m.database == nil {
		return result, nil
	}

	state, err := m.policyState(ctx, identity)
	if err != nil {
		return result, err
	}

	for _, template := range m.catalog.List() {
		decision := m.policy.Decide(policy.Request{Identity: identity, Template: template, State: state})

		switch {
		case decision.Allowed:
			result.Usable = append(result.Usable, summaryServer{Name: template.Name, Description: template.Spec.Description})
		case decision.Step == policy.StepWorkspaceScope && decision.Denied == "" && identity.Attestation == auth.AttestationAttested:
			result.Requestable = true
		}
	}

	return result, nil
}

// cleanText makes admin text safe for one line of a list: control and
// formatting characters and line breaks become spaces, runs of spaces
// collapse, and the result is cut to limit runes.
func cleanText(text string, limit int) string {
	text = strings.Map(func(char rune) rune {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) || unicode.IsSpace(char) || char == unicode.ReplacementChar {
			return ' '
		}

		return char
	}, text)
	text = strings.Join(strings.Fields(text), " ")

	if utf8.RuneCountInString(text) <= limit {
		return text
	}

	runes := []rune(text)

	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

// instructions is the text of the MCP instructions.
func (s catalogSummary) instructions() string {
	var out strings.Builder

	if len(s.Usable) == 0 {
		out.WriteString("No toolgate servers are usable in this session yet. Call `toolgate_list_servers` to see what you can ask for, or use the Workspaces page of the toolgate web UI.\n")

		return out.String()
	}

	out.WriteString("These MCP servers are available through toolgate:\n")

	more := 0

	for index, server := range s.Usable {
		line := "- " + cleanText(server.Name, 64)
		if description := cleanText(server.Description, summaryDescriptionRunes); description != "" {
			line += ": " + description
		}

		line += "\n"

		if index >= summaryMaxServers || out.Len()+len(line) > summaryMaxBytes-512 {
			more = len(s.Usable) - index

			break
		}

		out.WriteString(line)
	}

	if more > 0 {
		fmt.Fprintf(&out, "and %d more; use toolgate_find_tools\n", more)
	}

	if s.Requestable {
		out.WriteString("More servers may be available on request; call `toolgate_list_servers`.\n")
	}

	out.WriteString("To find a tool, call `toolgate_find_tools`, or call a server's tools directly.\n")

	return out.String()
}

// serverNames is the list of usable servers for a tool description,
// capped like the instructions.
func (s catalogSummary) serverNames() string {
	names := make([]string, 0, summaryMaxServers)

	for index, server := range s.Usable {
		if index >= summaryMaxServers {
			names = append(names, fmt.Sprintf("and %d more", len(s.Usable)-index))

			break
		}

		names = append(names, cleanText(server.Name, 64))
	}

	return strings.Join(names, ", ")
}

// findToolsDescription and listServersDescription are the descriptions of
// the two discovery meta tools. They name the usable servers, so a client
// that searches tool descriptions matches a query that names a server.
func (s catalogSummary) findToolsDescription() string {
	base := "Search the tools of the servers you can use by what you want to do. The tools it returns are added to your tool list."
	if names := s.serverNames(); names != "" {
		base = "Search the tools of: " + names + ". Search by what you want to do. The tools it returns are added to your tool list."
	}

	return base + " If a server is stopped, or search finds none of its tools yet, call toolgate_enable_server for it, then search again."
}

func (s catalogSummary) listServersDescription() string {
	base := "List the MCP servers you can enable, with their scope, status and whether they are enabled in this session. For a server that is stopped, call toolgate_enable_server."
	if names := s.serverNames(); names != "" {
		base += " Servers you can use now: " + names + "."
	}

	return base
}

// describeCatalog is receiving middleware that sets the instructions of
// the initialize and server/discover results for the request's identity.
func (m *metaTools) describeCatalog(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, request)
		if err != nil || (method != initializeMethod && method != mcprequest.DiscoverMethod) {
			return result, err
		}

		extra := request.GetExtra()
		if extra == nil || extra.TokenInfo == nil {
			return result, nil
		}

		current, ok := extra.TokenInfo.Extra[requestContextKey].(requestContext)
		if !ok {
			return result, nil
		}

		summary, sumErr := m.summary(ctx, current.identity)
		if sumErr != nil {
			return nil, sumErr
		}

		switch typed := result.(type) {
		case *mcp.InitializeResult:
			typed.Instructions = summary.instructions()
		case *mcp.DiscoverResult:
			typed.Instructions = summary.instructions()
		}

		return result, nil
	}
}

// syncMetaDescriptions re-registers the two discovery meta tools when their
// description changed, which sends list_changed to the session.
func (m *metaTools) syncMetaDescriptions(ctx context.Context, target *sessionTools, identity auth.Identity) error {
	find, list, err := m.metaDescriptions(ctx, identity)
	if err != nil {
		return err
	}

	m.setMetaDescriptions(target, find, list)

	return nil
}

// metaDescriptions reads the descriptions the two discovery meta tools
// should have for identity.
func (m *metaTools) metaDescriptions(ctx context.Context, identity auth.Identity) (find, list string, err error) {
	summary, err := m.summary(ctx, identity)
	if err != nil {
		return "", "", err
	}

	return summary.findToolsDescription(), summary.listServersDescription(), nil
}

// setMetaDescriptions re-registers the two discovery meta tools when find
// or list differ from their current descriptions. It reads nothing, so
// called right after the proxied tools are applied, both changes fall
// inside the go-sdk's list_changed debounce and send one notification.
func (m *metaTools) setMetaDescriptions(target *sessionTools, find, list string) {
	target.mu.Lock()
	defer target.mu.Unlock()

	if target.findDescription == find && target.listDescription == list {
		return
	}

	target.findDescription, target.listDescription = find, list
	m.registerDiscoveryTools(target.server, find, list)
}

// applySync makes target offer wanted and the discovery meta tools
// describe the servers identity can use. Every read happens before the
// first change, so a sync that changes both the proxied tools and the
// descriptions sends one list_changed, not one per part.
func (m *metaTools) applySync(ctx context.Context, target *sessionTools, identity auth.Identity, wanted map[string]*mcp.Tool) error {
	find, list, readErr := m.metaDescriptions(ctx, identity)

	// A failed summary read still applies the proxied tools: a policy
	// change must reach the list even when the descriptions can't.
	if err := target.apply(wanted); err != nil {
		return err
	}

	if readErr != nil {
		return readErr
	}

	m.setMetaDescriptions(target, find, list)

	return nil
}

func (m *metaTools) registerDiscoveryTools(server *mcp.Server, find, list string) {
	mcp.AddTool(server, &mcp.Tool{Name: ToolListServers, Description: list}, m.listServers)
	mcp.AddTool(server, &mcp.Tool{Name: ToolFindTools, Description: find}, m.findTools)
}
