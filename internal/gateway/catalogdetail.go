package gateway

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/toolcatalog"
)

// SecretStatus says whether a user has set one secret a server needs. It
// never holds the value.
type SecretStatus struct {
	Name string
	Set  bool
}

// ServerInfo is one catalog server as the web UI's Servers page shows it.
type ServerInfo struct {
	ServerEntry
	// Environments is the server's access.environments: any, opt-in or
	// attested.
	Environments string
	// Remote says toolgate connects to the server and doesn't start it.
	Remote bool
	// Secrets are the user secrets the server needs.
	Secrets []SecretStatus
	// ToolCount is the number of tools the user may see, or -1 when the
	// list isn't known yet.
	ToolCount int
}

// ToolInfo is one tool of a server as the Servers detail page shows it.
type ToolInfo struct {
	Name        string
	Description string
	// Environments is the tool's own override, empty when it follows the
	// server.
	Environments     string
	Blocked          bool
	ApprovalRequired bool
}

// Catalog returns the servers identity may see, with the details the
// Servers page shows. The servers are the ones Servers returns.
func (v *CatalogView) Catalog(ctx context.Context, identity auth.Identity) ([]ServerInfo, error) {
	entries, err := v.Servers(ctx, identity)
	if err != nil {
		return nil, err
	}

	stored, err := db.New(v.tools.database).ListUserSecretNames(ctx, identity.Principal)
	if err != nil {
		return nil, fmt.Errorf("read secrets: %w", err)
	}

	set := make(map[string]bool, len(stored))
	for _, row := range stored {
		set[row.Name] = true
	}

	current := requestContext{identity: identity}
	infos := make([]ServerInfo, 0, len(entries))

	for _, entry := range entries {
		template, err := v.tools.catalog.Offered(entry.Name)
		if err != nil {
			continue
		}

		info := ServerInfo{ServerEntry: entry, Environments: template.Spec.Access.Environments, Remote: template.Spec.Remote != nil, ToolCount: -1}

		for _, name := range catalog.RequiredSecretNames(template) {
			info.Secrets = append(info.Secrets, SecretStatus{Name: name, Set: set[name]})
		}

		if list, found, err := v.tools.curatedList(ctx, current, template); err != nil {
			return nil, err
		} else if found {
			info.ToolCount = len(list)
		}

		infos = append(infos, info)
	}

	return infos, nil
}

// ServerDetail returns one server's info and the tools identity may see in
// it. found is false when the server doesn't exist or identity has no access
// to it; the two look the same.
func (v *CatalogView) ServerDetail(ctx context.Context, identity auth.Identity, name string) (info ServerInfo, tools []ToolInfo, found bool, err error) {
	infos, err := v.Catalog(ctx, identity)
	if err != nil {
		return ServerInfo{}, nil, false, err
	}

	index := slices.IndexFunc(infos, func(each ServerInfo) bool { return each.Name == name })
	if index < 0 {
		return ServerInfo{}, nil, false, nil
	}

	template, err := v.tools.catalog.Offered(name)
	if err != nil {
		return ServerInfo{}, nil, false, nil //nolint:nilerr // A server that vanished is not found.
	}

	list, listed, err := v.tools.curatedList(ctx, requestContext{identity: identity}, template)
	if err != nil {
		return ServerInfo{}, nil, false, err
	}

	if listed {
		tools = toolInfos(template, list)
	}

	return infos[index], tools, true, nil
}

// toolInfos describes the curated tools of template, sorted by exposed name.
func toolInfos(template *v1alpha1.MCPServerTemplate, list map[string]toolcatalog.Tool) []ToolInfo {
	tools := make([]ToolInfo, 0, len(list))

	for upstream, tool := range list {
		override := template.Spec.Tools[upstream]
		tools = append(tools, ToolInfo{
			Name:             tool.ExposedName,
			Description:      tool.Tool.Description,
			Environments:     override.Environments,
			Blocked:          override.Blocked,
			ApprovalRequired: override.Approval == "required",
		})
	}

	slices.SortFunc(tools, func(a, b ToolInfo) int { return strings.Compare(a.Name, b.Name) })

	return tools
}
