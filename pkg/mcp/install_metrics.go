package mcp

import (
	"context"
	"io/fs"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
)

func init() { observability.SetCatalogSource("mcp_catalog_names", mcpCatalogNames) }

// Lowercased directory names of the optional MCP catalog, the public names install metrics may carry
func mcpCatalogNames() ([]string, error) {
	root, _ := catalogRoot()
	entries, err := fs.ReadDir(root, ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if st, err := fs.Stat(root, e.Name()); err == nil && st.IsDir() {
			names = append(names, pystr.Lower(e.Name()))
		}
	}
	return names, nil
}

// Name and failure class arguments of one install metric, empty strings passed as None
func extensionInstallArgs(name, failureClass string) (nameArg, classArg any) {
	if name != "" {
		nameArg = name
	}
	if failureClass != "" {
		classArg = failureClass
	}
	return nameArg, classArg
}

// Records one shared metrics extension install, a failed one classified by its class or its error
func recordExtensionInstall(ctx context.Context, kind, source, name, outcome, failureClass string, err error) {
	nameArg, classArg := extensionInstallArgs(name, failureClass)
	observability.RecordExtensionInstall(ctx, kind, source, nameArg, outcome, classArg, nil, err)
}
