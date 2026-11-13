package memory

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool environment of a home with its read only config, for plugin facing provider lookups
func homeEnv(ctx context.Context, h string) *tools.Env {
	cfg, _ := config.LoadReadonly(ctx, h, nil)
	cwd, _ := os.Getwd()
	return &tools.Env{Home: h, Config: cfg, Cwd: cwd, Log: slog.Default()}
}

// Provider directory a name resolves to in a home, false when the loader cannot resolve it
func ProviderDirForHome(h, name string) (string, bool) {
	ctx := context.Background()
	dir := FindProviderDir(ctx, homeEnv(ctx, h), name)
	return dir, dir != ""
}

const hostedMemoryProviderBase = "agent.memory_provider:MemoryProvider"

// Registration name of a memory provider object, false for any value that is not one
func HostedProviderName(obj any) (string, bool) {
	if p, ok := obj.(Provider); ok && p != nil {
		return p.Name(), true
	}
	hosted, ok := obj.(*plugins.HostedObject)
	if !ok || hosted == nil || hosted.Base != hostedMemoryProviderBase {
		return "", false
	}
	name, err := hosted.Get(context.Background(), "name")
	if err != nil {
		return "", false
	}
	return pyrepr.Str(name), true
}

// Curator subcommands with their help, in the hermes argparse order
var curatorSubcommandHelp = [][2]string{
	{"status", "Show curator status and skill stats"},
	{"usage", "Show usage telemetry for ALL skills (built-in, hub, agent) with provenance"},
	{"run", "Trigger a curator review now"},
	{"pause", "Pause the curator until resumed"},
	{"resume", "Resume a paused curator"},
	{"pin", "Pin a skill so the curator never auto-transitions it"},
	{"unpin", "Unpin a skill"},
	{"lock", "Lock a skill so the agent cannot change it (skill_manage, write_file, patch and terminal refuse; it stays readable and auto-loadable; your own edits are unaffected)"},
	{"unlock", "Lift a content lock"},
	{"list-unmanaged", "List curation-eligible skills with no provenance marker"},
	{"adopt", "Hand unmanaged skills to the curator (provenance is a user declaration)"},
	{"restore", "Restore an archived skill"},
	{"list-archived", "List archived skills"},
	{"archive", "Manually archive a skill (move to .archive/, excluded from prompt)"},
	{"prune", "Bulk-archive curator-managed skills idle for >= N days (default: curator.archive_after_days)"},
	{"backup", "Take a manual tar.gz snapshot of ICEBATH_HOME/skills/ (curator also does this automatically before every real run)"},
	{"rollback", "Restore ICEBATH_HOME/skills/ from a curator snapshot, or a single mutation by ledger entry id (see `icebath curator ledger`)"},
	{"ledger", "List the per-mutation skill audit ledger (all actors: curator/agent/user)"},
	{"purge", "Delete archived skills older than curator.archive_ttl_days (explicit only — never automatic; recorded in the ledger)"},
}

// Namespace value as text, empty for None
func nsText(ns map[string]any, key string) string {
	switch v := ns[key].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return pyrepr.Str(v)
	}
}

func nsBool(ns map[string]any, key string) bool { return config.PyTruthy(ns[key]) }

// Namespace integer, nil for None
func nsInt(ns map[string]any, key string) *int {
	switch v := ns[key].(type) {
	case int:
		return &v
	case int64:
		n := int(v)
		return &n
	case float64:
		n := int(v)
		return &n
	}
	return nil
}

// Namespace string list, every element rendered as text
func nsStrings(ns map[string]any, key string) []string {
	switch v := ns[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, pyrepr.Str(item))
		}
		return out
	case string:
		return []string{v}
	}
	return nil
}

// Curator arguments read from the argparse namespace of one subcommand
func curatorArgsFromNamespace(ns map[string]any) CuratorArgs {
	a := CuratorArgs{
		Skill: nsText(ns, "skill"), Skills: nsStrings(ns, "skill"), DryRun: nsBool(ns, "dry_run"), Background: nsBool(ns, "background"),
		Synchronous: nsBool(ns, "synchronous") || nsBool(ns, "sync"), Consolidate: nsBool(ns, "consolidate"),
		AllUnmanaged: nsBool(ns, "all_unmanaged"), Yes: nsBool(ns, "yes"), List: nsBool(ns, "list"), Compact: nsBool(ns, "compact"),
		JSON: nsBool(ns, "json"), Days: nsInt(ns, "days"), Reason: nsText(ns, "reason"), EntryID: nsText(ns, "entry_id"),
		BackupID: nsText(ns, "backup_id"), Limit: 20, Sort: nsText(ns, "sort"), Provenance: nsText(ns, "provenance"),
	}
	if limit := nsInt(ns, "limit"); limit != nil {
		a.Limit = *limit
	}
	if a.Sort == "" {
		a.Sort = "activity"
	}
	return a
}

// Runs icebath curator from its argparse namespace, the bare command printing the subcommand help
func CuratorCommand(ctx context.Context, env *tools.Env, ns map[string]any, w io.Writer) (int, error) {
	sub := nsText(ns, "curator_command")
	if sub == "" {
		fmt.Fprintln(w, "usage: icebath curator <subcommand>")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "subcommands:")
		for _, row := range curatorSubcommandHelp {
			fmt.Fprintf(w, "  %-16s %s\n", row[0], row[1])
		}
		return 0, nil
	}
	known := false
	for _, row := range curatorSubcommandHelp {
		known = known || row[0] == sub
	}
	if !known {
		fmt.Fprintf(w, "icebath curator: error: argument curator_command: invalid choice: %s\n", pyrepr.Repr(sub))
		return 2, nil
	}
	return NewCuratorCLI(w, w, os.Stdin).Run(tools.WithToolEnv(ctx, env), sub, curatorArgsFromNamespace(ns))
}
