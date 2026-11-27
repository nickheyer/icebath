package approvals

import (
	"cmp"
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	approvalsconfig "github.com/nickheyer/icebath/internal/approvals/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/tools/redact"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Commands the user kept approving are mined from the sessions into entries the allowlist could hold, never for classes too risky to allow

// Descriptions of risky classes never proposed, however often they were approved
var unsafeClass = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`delete`, `\brm\b`, `destro`, `wipe`, `format`, `\bdisk\b`, `block device`, `\bdd\b`, `fork bomb`, `kill (?:all )?process`, `kill all`,
	`self-termination`, `shutdown`, `reboot`, `\bsudo\b`, `privilege`, `credential`, `\bssh\b`, `shell.rc`, `system config`, `system file`,
	`\bsql\b`, `\bchown\b`, `\bchmod\b`, `writable`, `overwrite`, `in-place edit`, `pipe`, `obfuscation`, `remote content`, `remote script`,
	`heredoc`, `encoded`, `command substitution`, `process substitution`, `hardline`,
}, "|"))

// Programs a proposed glob never starts with
var unsafeRoots = strs.Set("rm", "rmdir", "unlink", "shred", "dd", "fdisk", "parted", "wipefs", "sudo", "doas", "su", "chmod", "chown", "chgrp", "kill",
	"killall", "pkill", "halt", "shutdown", "reboot", "poweroff", "init", "del", "format", "truncate", "mkswap")

// Text of a tool result saying the command did not run with the user's consent
var blockMarkers = []string{"BLOCKED (hardline)", "BLOCKED: User denied", "BLOCKED: Action ", "BLOCKED: Command flagged as dangerous",
	"BLOCKED: approval required", "BLOCKED: Failed to send approval request", "The user has NOT consented", "Asking the user for approval",
	"approval_required", "BLOCKED by user deny rule"}

// Entry the allowlist could hold, from the commands approved like it
type Proposal struct {
	Pattern string
	// glob for a command pattern, class for a kind of risky action
	Kind  string
	Count int
	// Kinds of risky action the commands were flagged as, sorted
	Classes []string
	// Up to three of the commands, shortened
	Examples []string
}

// A flagged terminal command that ran
type approvedCommand struct {
	command, description string
}

// Flagged terminal commands that ran in the profile's sessions active in the last days, every one when days is 0 or less
func (a *Approvals) history(ctx context.Context, cfg *config.Config, store sessions.Store, days int) ([]approvedCommand, error) {
	filter := &sessionsv1.Filter{Profile: cfg.Profile(), IncludeArchived: true, IncludeHidden: true, IncludeChildren: true, IncludeOpen: true,
		IncludePinned: true}
	var since time.Time
	if days > 0 {
		since = time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		filter.ActiveAfter = timestamppb.New(since)
	}
	list, _, err := store.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	r := reading(cfg)
	var out []approvedCommand
	for _, session := range list {
		messages, err := store.Messages(ctx, session.GetId(), sessions.All)
		if err != nil {
			return nil, err
		}
		blocked := map[string]bool{}
		type call struct{ id, command string }
		var calls []call
		for _, m := range messages {
			if !since.IsZero() && m.GetTime().AsTime().Before(since) {
				continue
			}
			switch m.GetRole() {
			case sessionsv1.Role_ROLE_TOOL:
				if text := m.GetText(); m.GetToolCallId() != "" && slices.ContainsFunc(blockMarkers, func(marker string) bool { return strings.Contains(text, marker) }) {
					blocked[m.GetToolCallId()] = true
				}
			case sessionsv1.Role_ROLE_ASSISTANT:
				for _, tc := range m.GetToolCalls() {
					if tc.GetName() != "terminal" {
						continue
					}
					var args struct {
						Command string `json:"command"`
					}
					if json.Unmarshal([]byte(cmp.Or(tc.GetArguments(), "{}")), &args) == nil && strings.TrimSpace(args.Command) != "" {
						calls = append(calls, call{tc.GetId(), args.Command})
					}
				}
			}
		}
		for _, c := range calls {
			if blocked[c.id] || r.hardline(c.command) != "" {
				continue
			}
			if description := r.dangerous(c.command); description != "" {
				out = append(out, approvedCommand{c.command, description})
			}
		}
	}
	return out, nil
}

// Command with the homes folded to their ~ forms and whitespace collapsed
func (r *reader) plain(command string) string {
	for _, f := range r.folds {
		tail := f.re.SubexpIndex("tail")
		command = f.re.ReplaceAllStringFunc(command, func(m string) string {
			return f.replacement + strings.ReplaceAll(f.re.FindStringSubmatch(m)[tail], `\`, "/")
		})
	}
	return strings.Join(strings.Fields(command), " ")
}

// Narrow glob for a simple command: the program, or the program and its subcommand, then anything; empty for a compound or risky one
func derive(command string) string {
	words := strings.Fields(command)
	if compound(command) || len(words) == 0 || unsafeRoot(words[0]) {
		return ""
	}
	if len(words) == 1 {
		return words[0]
	}
	if second := words[1]; strings.HasPrefix(second, "-") || strings.ContainsAny(second, "*?[$") {
		return words[0] + " *"
	}
	return words[0] + " " + words[1] + " *"
}

func unsafeRoot(word string) bool {
	name := strings.ToLower(word)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return unsafeRoots[name] || strings.HasPrefix(name, "mkfs")
}

// Proposals from the approval history of the profile's sessions active in the last days, the most approved first; entries the allowlist
// holds already, risky classes and patterns approved fewer than min times are left out
func (a *Approvals) Suggest(ctx context.Context, cfg *config.Config, store sessions.Store, days, least, limit int) ([]Proposal, error) {
	approved, err := a.history(ctx, cfg, store, days)
	if err != nil {
		return nil, err
	}
	existing := cfg.GetStringSlice(approvalsconfig.Allowlist)
	r := reading(cfg)
	type key struct{ pattern, kind string }
	byPattern := map[key]*Proposal{}
	var order []key
	for _, c := range approved {
		if unsafeClass.MatchString(c.description) {
			continue
		}
		raw := r.plain(c.command)
		shown := redact.File(raw, true)
		pattern, kind := derive(raw), "glob"
		if pattern != "" && derive(shown) != pattern {
			pattern = ""
		}
		if pattern == "" {
			pattern, kind = c.description, "class"
		}
		if slices.Contains(existing, pattern) {
			continue
		}
		k := key{pattern, kind}
		p := byPattern[k]
		if p == nil {
			p = &Proposal{Pattern: pattern, Kind: kind}
			byPattern[k] = p
			order = append(order, k)
		}
		p.Count++
		if !slices.Contains(p.Classes, c.description) {
			p.Classes = append(p.Classes, c.description)
		}
		example := strings.TrimSpace(shown)
		if runes := []rune(example); len(runes) > 100 {
			example = string(runes[:97]) + "..."
		}
		if !slices.Contains(p.Examples, example) && len(p.Examples) < 3 {
			p.Examples = append(p.Examples, example)
		}
	}
	var ranked []Proposal
	for _, k := range order {
		if p := byPattern[k]; p.Count >= max(least, 1) {
			sort.Strings(p.Classes)
			ranked = append(ranked, *p)
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Count != ranked[j].Count {
			return ranked[i].Count > ranked[j].Count
		}
		return ranked[i].Pattern < ranked[j].Pattern
	})
	return ranked[:min(len(ranked), max(limit, 1))], nil
}
