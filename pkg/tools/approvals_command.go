package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Namespace value as text, empty for None
func nsString(ns map[string]any, key string) string {
	switch v := ns[key].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return pyrepr.Str(v)
	}
}

// Namespace integer with a default for None
func nsIntOr(ns map[string]any, key string, def int) int {
	switch v := ns[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return def
}

// Namespace string list, every element rendered as text
func nsStringList(ns map[string]any, key string) []string {
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

// Python json.dumps of a value with the default separators
func pyDumps(v any, opts ...pyjson.Option) string {
	text, err := pyjson.Dumps(v, opts...)
	if err != nil {
		panic(err)
	}
	return text
}

// Dispatches icebath approvals <subcommand>, the usage text and exit 1 without one
func ApprovalsCommand(ctx context.Context, env *Env, ns map[string]any, w, errw io.Writer) (int, error) {
	switch nsString(ns, "approvals_command") {
	case "suggest":
		return suggestCommand(ctx, env, ns, w)
	case "test":
		return approvalsTestCommand(env, ns, w)
	}
	fmt.Fprint(w, "usage: icebath approvals <subcommand>\n"+
		"\n"+
		"subcommands:\n"+
		"  suggest    Mine past approval decisions into a proposed\n"+
		"             command_allowlist (dry by default; --apply N,M to merge)\n"+
		"  test       Dry-run the approval verdict for a command without\n"+
		"             executing it (exit 0 allow / 2 ask / 3 deny)\n"+
		"\n"+
		"Run `icebath approvals <subcommand> -h` for details.\n")
	return 1, nil
}

// Sorted class names of a proposal
func proposalClasses(p *Proposal) []string {
	classes := make([]string, 0, len(p.Classes))
	for c := range p.Classes {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	return classes
}

// Human readable proposal listing with the apply instructions
func renderProposals(w io.Writer, proposals []*Proposal, days int) {
	window := "all history"
	if days > 0 {
		window = fmt.Sprintf("last %d days", days)
	}
	if len(proposals) == 0 {
		fmt.Fprintf(w, "No allowlist candidates found in approval history (%s).\n"+
			"Either nothing dangerous was approved often enough "+
			"(see --min-count/--days), or the approved classes are excluded for safety.\n", window)
		return
	}
	fmt.Fprintf(w, "Proposed command_allowlist additions (from approval history, %s):\n\n", window)
	for i, p := range proposals {
		kind := ""
		if p.Kind == "class" {
			kind = " (class key)"
		}
		fmt.Fprintf(w, "  %d. %s    — approved %dx%s\n", i+1, p.Pattern, p.Count, kind)
		for _, cls := range proposalClasses(p) {
			fmt.Fprintf(w, "       class: %s\n", cls)
		}
		for _, ex := range p.Examples {
			fmt.Fprintf(w, "       e.g. %s\n", ex)
		}
	}
	fmt.Fprint(w, "\nNothing has been changed. Apply selected entries with:\n"+
		"  icebath approvals suggest --apply 1,3\n"+
		"Entries are merged into command_allowlist in ICEBATH_HOME/config.yaml.\n")
}

// icebath approvals suggest, proposing allowlist entries from the approval history and merging on --apply
func suggestCommand(ctx context.Context, env *Env, ns map[string]any, w io.Writer) (int, error) {
	dbPath := nsString(ns, "db")
	if dbPath == "" {
		dbPath = state.ResolveDefaultDBPath(ctx)
	}
	days := nsIntOr(ns, "days", 90)
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(w, "Session database not found: %s\n", dbPath)
		return 1, nil
	}
	records, err := ScanApprovalHistory(dbPath, days)
	if err != nil {
		return 1, err
	}
	proposals := BuildProposals(records, LoadPermanentAllowlist(env), nsIntOr(ns, "min_count", 2), nsIntOr(ns, "limit", 20))
	asJSON := config.PyTruthy(ns["json"])
	if applySpec := nsString(ns, "apply_indices"); applySpec != "" {
		indices, err := ParseApplyIndices(applySpec, len(proposals))
		if err != nil {
			fmt.Fprintf(w, "--apply error: %s\n", err.Error())
			return 1, nil
		}
		merged := ApplyProposals(env, proposals, indices)
		applied := []any{}
		for _, i := range indices {
			applied = append(applied, proposals[i].Pattern)
		}
		if asJSON {
			fmt.Fprintln(w, pyDumps(pyjson.ObjectOf("applied", applied, "allowlist_size", len(merged))))
		} else {
			fmt.Fprintln(w, "Added to command_allowlist:")
			for _, pattern := range applied {
				fmt.Fprintf(w, "  + %s\n", pattern)
			}
			fmt.Fprintf(w, "\ncommand_allowlist now has %d entries (ICEBATH_HOME/config.yaml).\n", len(merged))
		}
		return 0, nil
	}
	if asJSON {
		rows := []any{}
		for i, p := range proposals {
			rows = append(rows, pyjson.ObjectOf("n", i+1, "pattern", p.Pattern, "kind", p.Kind, "count", p.Count,
				"classes", jsonStringList(proposalClasses(p)), "examples", jsonStringList(p.Examples)))
		}
		fmt.Fprintln(w, pyDumps(pyjson.ObjectOf("db", dbPath, "days", days, "proposals", rows), pyjson.Indent(2)))
		return 0, nil
	}
	renderProposals(w, proposals, days)
	return 0, nil
}

// Strings as a JSON list value
func jsonStringList(items []string) []any {
	out := make([]any, 0, len(items))
	for _, s := range items {
		out = append(out, s)
	}
	return out
}

// icebath approvals test, the dry run verdict of a command never executed
func approvalsTestCommand(env *Env, ns map[string]any, w io.Writer) (int, error) {
	words := nsStringList(ns, "command_words")
	if len(words) > 0 && words[0] == "--" {
		words = words[1:]
	}
	if len(words) == 0 {
		fmt.Fprintln(w, "usage: icebath approvals test [--env-type TYPE] [--json] -- <command...>")
		return ExitUsage, nil
	}
	envType := nsString(ns, "env_type")
	if envType == "" {
		envType = "local"
	}
	verdict := EvaluateCommand(env, strings.Join(words, " "), envType)
	if config.PyTruthy(ns["json"]) {
		rule, detail := any(nil), any(nil)
		if verdict.Rule != "" {
			rule = verdict.Rule
		}
		if verdict.Detail != "" {
			detail = verdict.Detail
		}
		fmt.Fprintln(w, pyDumps(pyjson.ObjectOf("command", verdict.Command, "env_type", verdict.EnvType, "verdict", verdict.Verdict,
			"exit_code", verdict.ExitCode, "rule", rule, "detail", detail, "normalized_variants", jsonStringList(verdict.NormalizedVariants)), pyjson.Indent(2)))
	} else {
		fmt.Fprint(w, RenderVerdict(verdict))
	}
	return verdict.ExitCode, nil
}
