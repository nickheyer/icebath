package provider

import (
	"context"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Copies a string list into the JSON list json.dumps prints for it, empty when the list is nil
func codexReportList(items []string) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}

// The None or str value of an optional report field
func codexReportOptional(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// CodexRuntimeCommand runs the codex config migration for the profile home and prints its report, exit one on any error
func CodexRuntimeCommand(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		return 1, err
	}
	report, err := MigrateCodexRuntimePlugins(ctx, cfg, CodexMigrateOptions{DryRun: pyrepr.Truthy(ns["dry_run"])})
	if err != nil {
		return 1, err
	}
	if pyrepr.Truthy(ns["json"]) {
		target := report.TargetPath
		if target == "" {
			target = "None"
		}
		skipped := map[string]any{}
		for name, keys := range report.SkippedKeysPerServer {
			skipped[name] = codexReportList(keys)
		}
		text, err := pyjson.Dumps(map[string]any{
			"target_path":               target,
			"migrated":                  codexReportList(report.Migrated),
			"skipped_keys_per_server":   skipped,
			"migrated_plugins":          codexReportList(report.MigratedPlugins),
			"plugin_query_error":        codexReportOptional(report.PluginQueryError),
			"wrote_permissions_default": codexReportOptional(report.WrotePermissionsDefault),
			"preserved_user_servers":    codexReportList(report.PreservedUserServers),
			"errors":                    codexReportList(report.Errors),
			"written":                   report.Written,
			"dry_run":                   report.DryRun,
		}, pyjson.Indent(2), pyjson.SortKeys(true))
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(w, text)
	} else {
		fmt.Fprintln(w, report.Summary())
	}
	if len(report.Errors) > 0 {
		return 1, nil
	}
	return 0, nil
}
