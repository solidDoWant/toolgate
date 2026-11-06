package catalog

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
)

// Start values of a template.
const (
	StartOnCall   = "onCall"
	StartOnEnable = "onEnable"
)

// ReservedServerName is the name of the built-in server. A template with it
// is invalid and counts as deleted.
const ReservedServerName = "toolgate"

const reservedNameReason = "The server name `" + ReservedServerName + "` is reserved for the built-in server."

// Curation bounds (docs/design/phase-4.md §6). The CRD's CEL rules check the
// same ones.
const (
	maxFilterNames       = 512
	maxFilterNameLength  = 128
	maxDescription       = 4096
	maxInputDescriptions = 64
	maxOverrides         = 256
)

// validateTools checks the fields that start, declare and curate tools.
func validateTools(template *v1alpha1.MCPServerTemplate) []string {
	spec := template.Spec

	var problems []string

	if !slices.Contains([]string{"", StartOnCall, StartOnEnable}, spec.Start) {
		problems = append(problems, fmt.Sprintf("start %q must be one of onCall, onEnable", spec.Start))
	}

	problems = append(problems, validateToolFilter(spec.ToolFilter)...)
	problems = append(problems, validateOverrides(template)...)
	problems = append(problems, validateDeclaredTools(template)...)

	return problems
}

func validateToolFilter(filter *v1alpha1.ToolFilter) []string {
	if filter == nil {
		return nil
	}

	var problems []string

	if (filter.Allow != nil) == (filter.Deny != nil) {
		problems = append(problems, "`toolFilter` must set exactly one of `allow` and `deny`.")
	}

	for field, names := range map[string][]string{"allow": filter.Allow, "deny": filter.Deny} {
		if len(names) > maxFilterNames {
			problems = append(problems, fmt.Sprintf("`toolFilter.%s` has %d names, more than the limit of %d.", field, len(names), maxFilterNames))
		}

		for _, name := range names {
			if len(name) > maxFilterNameLength {
				problems = append(problems, fmt.Sprintf("`toolFilter.%s` has a name longer than %d characters.", field, maxFilterNameLength))

				break
			}
		}
	}

	slices.Sort(problems)

	return problems
}

func validateOverrides(template *v1alpha1.MCPServerTemplate) []string {
	var problems []string

	if len(template.Spec.Tools) > maxOverrides {
		problems = append(problems, fmt.Sprintf("`tools` has %d entries, more than the limit of %d.", len(template.Spec.Tools), maxOverrides))
	}

	upstreams := slices.Sorted(maps.Keys(template.Spec.Tools))
	owners := map[string]string{}

	for _, upstream := range upstreams {
		override := template.Spec.Tools[upstream]

		if override.Name != "" {
			if first, found := owners[override.Name]; found {
				problems = append(problems, fmt.Sprintf("Tool name `%s` is the name of both `%s` and `%s`.", override.Name, first, upstream))
			} else {
				owners[override.Name] = upstream
			}

			switch {
			case !toolcatalog.ExposedNamePattern.MatchString(override.Name):
				problems = append(problems, fmt.Sprintf("`tools.%s.name` must match %s.", upstream, toolcatalog.ExposedNamePattern))
			case len(template.Name)+len("__")+len(override.Name) > toolcatalog.MaxToolNameLength:
				problems = append(problems, fmt.Sprintf("Tool `%s` is longer than %d characters with its `%s__` prefix.",
					override.Name, toolcatalog.MaxToolNameLength, template.Name))
			}

			if filter := template.Spec.ToolFilter; filter != nil && slices.Contains(filter.Deny, override.Name) {
				problems = append(problems, fmt.Sprintf("Tool name `%s` of `%s` is the name of a hidden tool.", override.Name, upstream))
			}
		}

		if len(override.Description) > maxDescription {
			problems = append(problems, fmt.Sprintf("`tools.%s.description` is longer than %d characters.", upstream, maxDescription))
		}

		if len(override.InputDescriptions) > maxInputDescriptions {
			problems = append(problems, fmt.Sprintf("`tools.%s.inputDescriptions` has more than %d entries.", upstream, maxInputDescriptions))
		}

		for _, description := range override.InputDescriptions {
			if len(description) > maxDescription {
				problems = append(problems, fmt.Sprintf("`tools.%s.inputDescriptions` has a description longer than %d characters.", upstream, maxDescription))

				break
			}
		}
	}

	slices.Sort(problems)

	return problems
}

func validateDeclaredTools(template *v1alpha1.MCPServerTemplate) []string {
	declared := template.Spec.DeclaredTools
	if len(declared) == 0 {
		return nil
	}

	if len(declared) > toolcatalog.MaxTools {
		return []string{fmt.Sprintf("`declaredTools` has %d tools, more than the limit of %d.", len(declared), toolcatalog.MaxTools)}
	}

	tools := make([]*mcp.Tool, 0, len(declared))
	names := map[string]bool{}

	for _, item := range declared {
		if names[item.Name] {
			return []string{fmt.Sprintf("`declaredTools` names `%s` twice.", item.Name)}
		}

		names[item.Name] = true

		tool := &mcp.Tool{Name: item.Name, Description: item.Description, InputSchema: map[string]any{"type": "object"}}

		if item.InputSchema != nil && len(item.InputSchema.Raw) > 0 {
			var schema map[string]any
			if err := json.Unmarshal(item.InputSchema.Raw, &schema); err != nil || schema == nil {
				return []string{fmt.Sprintf("`declaredTools` item `%s` has an `inputSchema` that is not an object.", item.Name)}
			}

			tool.InputSchema = schema
		}

		tools = append(tools, tool)
	}

	encoded, err := json.Marshal(tools)
	if err != nil || len(encoded) > toolcatalog.MaxDeclaredToolsBytes {
		return []string{fmt.Sprintf("`declaredTools` is %d bytes, more than the limit of 1 MiB.", len(encoded))}
	}

	if reason := toolcatalog.Check(template, tools); reason != "" {
		return []string{reason}
	}

	return nil
}

// validateReservedName reports the reserved server name.
func validateReservedName(template *v1alpha1.MCPServerTemplate) []string {
	if strings.EqualFold(template.Name, ReservedServerName) {
		return []string{reservedNameReason}
	}

	return nil
}
