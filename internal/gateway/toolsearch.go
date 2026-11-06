package gateway

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
)

// ToolFindTools is the name of the search tool of search mode.
const ToolFindTools = "toolgate_find_tools"

// Limits of tool search (docs/design/phase-4.md §5).
const (
	maxQueryChars    = 2000
	maxQueryWords    = 64
	maxFindLimit     = 20
	defaultFindLimit = 5
	// MaxFoundTools is how many found tools a session keeps; a new one
	// pushes out the oldest.
	MaxFoundTools = 50
	// searchTimeout is the statement_timeout of the ranking query.
	searchTimeout = 2 * time.Second
)

const (
	messageSearchOff = "Tool search is off for this session."
	messageNoWords   = "The query has no words to search for. Describe the task in English words."
)

// Values of the access field of a found tool.
const (
	accessAllowed = "allowed"
	accessAsk     = "ask"
)

// FindToolsInput is the toolgate_find_tools input.
type FindToolsInput struct {
	Query string `json:"query" jsonschema:"what you want to do, in English words, 1 to 2000 characters"`
	Limit int    `json:"limit,omitempty" jsonschema:"how many tools to return, 1 to 20; 5 when left out"`
}

// FoundTool is one tool in the toolgate_find_tools result.
type FoundTool struct {
	Name        string `json:"name"`
	Server      string `json:"server"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
	// Access is `allowed`, or `ask` for a tool the first call of which asks
	// the user to add its server to the session.
	Access string `json:"access"`
	// Approval says each call of the tool needs a person's approval.
	Approval bool `json:"approval"`
}

// FindToolsResult is the toolgate_find_tools result.
type FindToolsResult struct {
	Tools []FoundTool `json:"tools"`
}

var queryWordPattern = regexp.MustCompile(`[a-z0-9]+`)

// queryWords returns the lowercased words of [a-z0-9] in query, at most
// maxQueryWords. They hold no tsquery syntax.
func queryWords(query string) []string {
	words := queryWordPattern.FindAllString(strings.ToLower(query), maxQueryWords)
	if words == nil {
		return nil
	}

	return words
}

// tsQuery is the text passed to to_tsquery: the words joined with ` | `.
func tsQuery(words []string) string {
	return strings.Join(words, " | ")
}

// asksForTool reports whether decision refuses only at the workspace-scope
// step, in a verified workspace, with no deny entry: then the tool is shown
// with access `ask`, and its first call asks the user.
func asksForTool(current requestContext, decision policy.Decision) bool {
	return !decision.Allowed && decision.Step == policy.StepWorkspaceScope && decision.Denied == "" &&
		VerifiedWorkspace(current.identity) != ""
}

// curatedList returns the template's curated catalog list keyed by upstream
// name, and false when it has no list.
func (m *metaTools) curatedList(ctx context.Context, template *v1alpha1.MCPServerTemplate) (map[string]toolcatalog.Tool, bool, error) {
	upstream, found, err := m.catalogList(ctx, template)
	if err != nil || !found {
		return nil, false, err
	}

	curated := toolcatalog.Curate(template, upstream)
	byUpstream := make(map[string]toolcatalog.Tool, len(curated))

	for _, tool := range curated {
		byUpstream[tool.UpstreamName] = tool
	}

	return byUpstream, true, nil
}

func proxiedFrom(template *v1alpha1.MCPServerTemplate, curated toolcatalog.Tool) *mcp.Tool {
	proxied := *curated.Tool
	proxied.Name = ProxiedToolName(template.Name, curated.ExposedName)

	if proxied.InputSchema == nil {
		proxied.InputSchema = map[string]any{"type": "object"}
	}

	return &proxied
}

func (m *metaTools) findTools(ctx context.Context, request *mcp.CallToolRequest, input FindToolsInput) (*mcp.CallToolResult, FindToolsResult, error) {
	current, ok := requestContextOf(request)
	if !ok {
		return nil, FindToolsResult{}, errNoIdentity
	}

	if !current.searchMode {
		return nil, FindToolsResult{}, errors.New(messageSearchOff) //nolint:staticcheck // A sentence for the model.
	}

	if chars := utf8.RuneCountInString(input.Query); chars < 1 || chars > maxQueryChars {
		return nil, FindToolsResult{}, fmt.Errorf("The query must be 1 to %d characters.", maxQueryChars) //nolint:staticcheck // A sentence for the model.
	}

	limit := input.Limit
	if limit == 0 {
		limit = defaultFindLimit
	}

	if limit < 1 || limit > maxFindLimit {
		return nil, FindToolsResult{}, fmt.Errorf("The limit must be 1 to %d.", maxFindLimit) //nolint:staticcheck // A sentence for the model.
	}

	empty := FindToolsResult{Tools: []FoundTool{}}
	noWords := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: messageNoWords}}}

	words := queryWords(input.Query)
	if len(words) == 0 {
		return noWords, empty, nil
	}

	state, err := m.policyState(ctx, current.identity)
	if err != nil {
		return nil, FindToolsResult{}, err
	}

	// Candidates: servers the server-level decision allows, or refuses only
	// because they are outside the list, in a verified workspace.
	candidates := map[string]*v1alpha1.MCPServerTemplate{}
	uids := []string{}

	for _, template := range m.catalog.List() {
		decision := m.policy.Decide(policy.Request{Identity: current.identity, Template: template, State: state})
		if decision.Allowed || asksForTool(current, decision) {
			candidates[string(template.UID)] = template
			uids = append(uids, string(template.UID))
		}
	}

	rows, stopWordsOnly, err := m.searchRows(ctx, uids, tsQuery(words), tsQuery(expandWords(words)), strings.Join(words, " "))
	if err != nil {
		return nil, FindToolsResult{}, err
	}

	if stopWordsOnly {
		return noWords, empty, nil
	}

	lists := map[string]map[string]toolcatalog.Tool{}
	result := FindToolsResult{Tools: []FoundTool{}}
	kept := []db.SearchToolsRow{}

	for _, row := range rows {
		if len(result.Tools) >= limit {
			break
		}

		template := candidates[row.TemplateUid]
		if template == nil {
			continue
		}

		list, loaded := lists[row.TemplateUid]
		if !loaded {
			list, _, err = m.curatedList(ctx, template)
			if err != nil {
				return nil, FindToolsResult{}, err
			}

			lists[row.TemplateUid] = list
		}

		curated, present := list[row.UpstreamName]
		if !present {
			continue
		}

		decision := m.policy.Decide(policy.Request{
			Identity: current.identity, Template: template, Tool: curated.UpstreamName,
			ExposedTool: curated.ExposedName, State: state,
		})

		access := accessAllowed

		switch {
		case decision.Allowed:
		case asksForTool(current, decision):
			access = accessAsk
		default:
			continue
		}

		proxied := proxiedFrom(template, curated)

		result.Tools = append(result.Tools, FoundTool{
			Name: proxied.Name, Server: template.Name, Description: proxied.Description,
			InputSchema: proxied.InputSchema, Access: access,
			Approval: template.Spec.Tools[curated.UpstreamName].Approval == policy.ApprovalRequired,
		})

		kept = append(kept, row)
	}

	if err := m.recordFound(ctx, request, current, kept); err != nil {
		return nil, FindToolsResult{}, err
	}

	return nil, result, nil
}

// searchRows runs the ranking query in a read-only transaction with a
// statement timeout. stopWordsOnly is true when every word of the query is a
// stop word, so the query is empty and matches nothing.
func (m *metaTools) searchRows(ctx context.Context, uids []string, query, expanded, plain string) (rows []db.SearchToolsRow, stopWordsOnly bool, err error) {
	err = pgx.BeginTxFunc(ctx, m.database, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", searchTimeout.Milliseconds())); err != nil {
			return fmt.Errorf("set the search timeout: %w", err)
		}

		queries := db.New(tx)

		empty, err := queries.QueryIsEmpty(ctx, query)
		if err != nil {
			return fmt.Errorf("parse the query: %w", err)
		}

		if empty {
			stopWordsOnly = true

			return nil
		}

		if len(uids) == 0 {
			return nil
		}

		rows, err = queries.SearchTools(ctx, db.SearchToolsParams{Query: query, Expanded: expanded, Plain: plain, TemplateUids: uids})
		if err != nil {
			return fmt.Errorf("search the tools: %w", err)
		}

		return nil
	})

	return rows, stopWordsOnly, err
}

// recordFound writes the found tools in one transaction that locks the
// session row, pushes out the oldest beyond MaxFoundTools, and drops rows of
// templates that are no longer offered. When the set changed, it notifies
// the session's streams, and syncs this MCP session at once.
func (m *metaTools) recordFound(ctx context.Context, request *mcp.CallToolRequest, current requestContext, found []db.SearchToolsRow) error {
	if len(found) == 0 {
		return nil
	}

	changed := false

	err := pgx.BeginFunc(ctx, m.database, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if _, err := queries.LockSession(ctx, current.session.ID); err != nil {
			return fmt.Errorf("lock session: %w", err)
		}

		before, err := queries.ListSessionFoundTools(ctx, current.session.ID)
		if err != nil {
			return fmt.Errorf("read found tools: %w", err)
		}

		for _, row := range before {
			template, offerErr := m.catalog.Offered(row.TemplateName)
			if offerErr == nil && template.Namespace == row.TemplateNs {
				continue
			}

			err := queries.DeleteSessionFoundTool(ctx, db.DeleteSessionFoundToolParams{
				SessionPk: current.session.ID, TemplateNs: row.TemplateNs, TemplateName: row.TemplateName, UpstreamName: row.UpstreamName,
			})
			if err != nil {
				return fmt.Errorf("delete a found tool: %w", err)
			}
		}

		for _, row := range found {
			err := queries.TouchSessionFoundTool(ctx, db.TouchSessionFoundToolParams{
				SessionPk: current.session.ID, TemplateNs: row.TemplateNs, TemplateName: row.TemplateName, UpstreamName: row.UpstreamName,
			})
			if err != nil {
				return fmt.Errorf("write a found tool: %w", err)
			}
		}

		if err := queries.TrimSessionFoundTools(ctx, db.TrimSessionFoundToolsParams{SessionPk: current.session.ID, Keep: MaxFoundTools}); err != nil {
			return fmt.Errorf("trim found tools: %w", err)
		}

		after, err := queries.ListSessionFoundTools(ctx, current.session.ID)
		if err != nil {
			return fmt.Errorf("read found tools: %w", err)
		}

		changed = !sameFound(before, after)
		if !changed {
			return nil
		}

		return store.Notify(ctx, tx, store.KindSession, fmt.Sprint(current.session.ID))
	})
	if err != nil {
		return err
	}

	if changed {
		if _, err := m.syncSession(ctx, request.Session, current); err != nil {
			return fmt.Errorf("update the session's tools: %w", err)
		}
	}

	return nil
}

func sameFound(before, after []db.SessionFoundTool) bool {
	if len(before) != len(after) {
		return false
	}

	for index := range before {
		if before[index].TemplateNs != after[index].TemplateNs || before[index].TemplateName != after[index].TemplateName ||
			before[index].UpstreamName != after[index].UpstreamName {
			return false
		}
	}

	return true
}

// addFoundTools adds the session's found tools to tools in search mode: each
// one the policy allows, or refuses only because its server is outside the
// list (access `ask`). A found tool whose template or tool is gone is
// left out.
func (m *metaTools) addFoundTools(ctx context.Context, current requestContext, tools map[string]*mcp.Tool, byServer map[string][]string) error {
	rows, err := db.New(m.database).ListSessionFoundTools(ctx, current.session.ID)
	if err != nil {
		return fmt.Errorf("read found tools: %w", err)
	}

	if len(rows) == 0 {
		return nil
	}

	state, err := m.policyState(ctx, current.identity)
	if err != nil {
		return err
	}

	lists := map[string]map[string]toolcatalog.Tool{}

	for _, row := range rows {
		template, offerErr := m.catalog.Offered(row.TemplateName)
		if offerErr != nil || template.Namespace != row.TemplateNs {
			continue
		}

		key := row.TemplateNs + "/" + row.TemplateName

		list, loaded := lists[key]
		if !loaded {
			list, _, err = m.curatedList(ctx, template)
			if err != nil {
				return err
			}

			lists[key] = list
		}

		curated, present := list[row.UpstreamName]
		if !present {
			continue
		}

		decision := m.policy.Decide(policy.Request{
			Identity: current.identity, Template: template, Tool: curated.UpstreamName,
			ExposedTool: curated.ExposedName, State: state,
		})
		if !decision.Allowed && !asksForTool(current, decision) {
			continue
		}

		proxied := proxiedFrom(template, curated)
		if _, exists := tools[proxied.Name]; exists {
			continue
		}

		tools[proxied.Name] = proxied
		byServer[template.Name] = append(byServer[template.Name], proxied.Name)
	}

	return nil
}
