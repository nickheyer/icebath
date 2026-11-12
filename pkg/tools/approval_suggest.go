package tools

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	_ "modernc.org/sqlite"
)

// Dangerous class descriptions that are never proposed regardless of approval frequency
var unsafeClassRe = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`delete`, `\brm\b`, `destro`, `wipe`, `format`, `\bdisk\b`, `block device`, `\bdd\b`,
	`fork bomb`, `kill (?:all )?process`, `kill all`, `self-termination`, `shutdown`, `reboot`,
	`\bsudo\b`, `privilege`, `credential`, `\bssh\b`, `shell.rc`, `system config`, `system file`,
	`\bsql\b`, `\bchown\b`, `\bchmod\b`, `writable`, `overwrite`, `in-place edit`,
	`pipe`, `obfuscation`, `remote content`, `remote script`, `heredoc`, `encoded`,
	`command substitution`, `process substitution`,
	`hardline`,
}, "|"))

var unsafeRootBinaries = map[string]bool{
	"rm": true, "rmdir": true, "unlink": true, "shred": true, "dd": true, "fdisk": true, "parted": true, "wipefs": true,
	"sudo": true, "doas": true, "su": true, "chmod": true, "chown": true, "chgrp": true,
	"kill": true, "killall": true, "pkill": true,
	"halt": true, "shutdown": true, "reboot": true, "poweroff": true, "init": true,
	"del": true, "format": true, "truncate": true, "mkswap": true,
}

var unsafeRootPrefixes = []string{"mkfs"}

// Tool result substrings meaning the command did not execute with user consent
var blockMarkers = []string{
	"BLOCKED (hardline)", "BLOCKED: User denied", "BLOCKED: Action ",
	"BLOCKED: Command flagged as dangerous", "BLOCKED: approval required",
	"BLOCKED: Failed to send approval request", "The user has NOT consented",
	"Asking the user for approval", "approval_required", "BLOCKED by user deny rule",
}

// Proposal is one ranked allowlist proposal mined from approval history
type Proposal struct {
	Pattern  string
	Kind     string
	Count    int
	Classes  map[string]bool
	Examples []string
}

// Adds a shortened example command, at most three distinct ones
func (p *Proposal) addExample(command string) {
	short := strings.TrimSpace(command)
	if len([]rune(short)) > 100 {
		short = string([]rune(short)[:97]) + "..."
	}
	for _, e := range p.Examples {
		if e == short {
			return
		}
	}
	if len(p.Examples) < 3 {
		p.Examples = append(p.Examples, short)
	}
}

// SortedClasses lists the dangerous classes of a proposal in order
func (p *Proposal) SortedClasses() []string {
	out := make([]string, 0, len(p.Classes))
	for c := range p.Classes {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// ApprovalRecord is one dangerous classified terminal command that actually ran
type ApprovalRecord struct {
	Command     string
	Description string
}

// DefaultApprovalDBPath is the state database of the environment's home
func DefaultApprovalDBPath(env *Env) string {
	if env != nil && env.Home != "" {
		return home.StatePath(env.Home)
	}
	return home.StatePath(home.Process())
}

// Decodes JSON text, ok false on malformed input
func jsonOrNone(raw string) (any, bool) {
	v, err := pyjson.DecodeWTF8([]byte(raw))
	if err != nil {
		return nil, false
	}
	return v, true
}

type terminalCall struct {
	id      string
	command string
}

// Every terminal tool call of an assistant message since the timestamp
func iterTerminalCalls(db *sql.DB, sinceTS float64) ([]terminalCall, error) {
	rows, err := db.Query("SELECT tool_calls FROM messages WHERE role='assistant' AND tool_calls IS NOT NULL "+
		"AND tool_calls LIKE '%terminal%' AND timestamp >= ?", sinceTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []terminalCall
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		calls, ok := jsonOrNone(raw.String)
		list, _ := calls.([]any)
		if !ok || list == nil {
			continue
		}
		for _, item := range list {
			call, _ := item.(map[string]any)
			fn, _ := pyjson.Dict(call["function"])
			if fn["name"] != "terminal" {
				continue
			}
			argsRaw, _ := fn["arguments"].(string)
			if argsRaw == "" {
				argsRaw = "{}"
			}
			args, ok := jsonOrNone(argsRaw)
			argMap, _ := args.(map[string]any)
			if !ok || argMap == nil {
				continue
			}
			command, _ := argMap["command"].(string)
			if strings.TrimSpace(command) == "" {
				continue
			}
			id, _ := call["id"].(string)
			out = append(out, terminalCall{id: id, command: command})
		}
	}
	return out, rows.Err()
}

// Tool call ids whose result shows the command never ran freely
func blockedToolCallIDs(db *sql.DB, sinceTS float64) (map[string]bool, error) {
	rows, err := db.Query("SELECT tool_call_id, content FROM messages "+
		"WHERE role='tool' AND tool_call_id IS NOT NULL AND timestamp >= ? "+
		"AND (content LIKE '%BLOCKED%' OR content LIKE '%approval%')", sinceTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	blocked := map[string]bool{}
	for rows.Next() {
		var id, content sql.NullString
		if err := rows.Scan(&id, &content); err != nil {
			return nil, err
		}
		for _, marker := range blockMarkers {
			if content.Valid && strings.Contains(content.String, marker) {
				blocked[id.String] = true
				break
			}
		}
	}
	return blocked, rows.Err()
}

// ScanApprovalHistory lists dangerous classified terminal commands that executed with implied consent
func ScanApprovalHistory(dbPath string, days int) ([]ApprovalRecord, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	sinceTS := 0.0
	if days > 0 {
		sinceTS = float64(time.Now().Unix()) - float64(days)*86400
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	blocked, err := blockedToolCallIDs(db, sinceTS)
	if err != nil {
		return nil, err
	}
	calls, err := iterTerminalCalls(db, sinceTS)
	if err != nil {
		return nil, err
	}
	var records []ApprovalRecord
	for _, call := range calls {
		if blocked[call.id] {
			continue
		}
		if hardline, _ := DetectHardlineCommand(call.command); hardline {
			continue
		}
		if _, description, dangerous := DetectDangerousCommand(call.command); dangerous {
			records = append(records, ApprovalRecord{Command: call.command, Description: description})
		}
	}
	return records, nil
}

// NormalizeCommand folds user and icebath home prefixes and collapses whitespace
func NormalizeCommand(command string) string {
	return strings.Join(strings.Fields(rewriteResolvedUserHome(rewriteResolvedIcebathHome(command))), " ")
}

// IsUnsafeClass reports a dangerous class description that must never be proposed
func IsUnsafeClass(description string) bool { return unsafeClassRe.MatchString(description) }

// True when a root binary must never anchor a proposed glob
func unsafeRootBinary(token string) bool {
	tok := strings.ToLower(token)
	if i := strings.LastIndex(tok, "/"); i >= 0 {
		tok = tok[i+1:]
	}
	if unsafeRootBinaries[tok] {
		return true
	}
	for _, p := range unsafeRootPrefixes {
		if strings.HasPrefix(tok, p) {
			return true
		}
	}
	return false
}

// DeriveGlob derives a narrow command glob from a simple command, empty for compound or unsafe commands
func DeriveGlob(normalized string) string {
	tokens := strings.Fields(normalized)
	if hasAllowlistShellOperator(normalized) || len(tokens) == 0 || unsafeRootBinary(tokens[0]) {
		return ""
	}
	if len(tokens) == 1 {
		return tokens[0]
	}
	second := tokens[1]
	if strings.HasPrefix(second, "-") || strings.ContainsAny(second, "*?[$") {
		return tokens[0] + " *"
	}
	return tokens[0] + " " + second + " *"
}

// BuildProposals aggregates scan records into a ranked, safety filtered proposal list
func BuildProposals(records []ApprovalRecord, existing map[string]bool, minCount, limit int) []*Proposal {
	type key struct{ pattern, kind string }
	byPattern := map[key]*Proposal{}
	for _, rec := range records {
		if IsUnsafeClass(rec.Description) {
			continue
		}
		raw := NormalizeCommand(rec.Command)
		normalized := RedactSensitiveText(raw)
		glob := DeriveGlob(raw)
		if glob != "" && DeriveGlob(normalized) != glob {
			glob = ""
		}
		pattern, kind := glob, "glob"
		if glob == "" {
			pattern, kind = rec.Description, "class"
		}
		if existing[pattern] {
			continue
		}
		k := key{pattern, kind}
		proposal := byPattern[k]
		if proposal == nil {
			proposal = &Proposal{Pattern: pattern, Kind: kind, Classes: map[string]bool{}}
			byPattern[k] = proposal
		}
		proposal.Count++
		proposal.Classes[rec.Description] = true
		proposal.addExample(normalized)
	}
	if minCount < 1 {
		minCount = 1
	}
	var ranked []*Proposal
	for _, p := range byPattern {
		if p.Count >= minCount {
			ranked = append(ranked, p)
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Count != ranked[j].Count {
			return ranked[i].Count > ranked[j].Count
		}
		return ranked[i].Pattern < ranked[j].Pattern
	})
	if limit < 1 {
		limit = 1
	}
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked
}

// ParseApplyIndices parses a selection like 1,3 into validated zero based indices
func ParseApplyIndices(spec string, total int) ([]int, error) {
	var indices []int
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid selection '%s' — expected numbers like 1,3", part)
		}
		if n < 1 || n > total {
			return nil, fmt.Errorf("selection %d out of range (1..%d)", n, total)
		}
		seen := false
		for _, existing := range indices {
			if existing == n-1 {
				seen = true
			}
		}
		if !seen {
			indices = append(indices, n-1)
		}
	}
	if len(indices) == 0 {
		return nil, errors.New("no valid selections in --apply")
	}
	return indices, nil
}

// ApplyProposals merges the chosen proposal patterns into command_allowlist and persists them
func ApplyProposals(env *Env, proposals []*Proposal, indices []int) map[string]bool {
	merged := copySet(LoadPermanentAllowlist(env))
	for _, idx := range indices {
		merged[proposals[idx].Pattern] = true
	}
	SavePermanentAllowlist(env, merged)
	LoadPermanent(env, merged)
	return merged
}

// RenderProposals formats proposals the way the approvals suggest command prints them
func RenderProposals(proposals []*Proposal, days int) string {
	window := "all history"
	if days > 0 {
		window = fmt.Sprintf("last %d days", days)
	}
	var b strings.Builder
	if len(proposals) == 0 {
		fmt.Fprintf(&b, "No allowlist candidates found in approval history (%s).\nEither nothing dangerous was approved often enough (see --min-count/--days), or the approved classes are excluded for safety.\n", window)
		return b.String()
	}
	fmt.Fprintf(&b, "Proposed command_allowlist additions (from approval history, %s):\n\n", window)
	for i, p := range proposals {
		kind := ""
		if p.Kind == "class" {
			kind = " (class key)"
		}
		fmt.Fprintf(&b, "  %d. %s    — approved %dx%s\n", i+1, p.Pattern, p.Count, kind)
		for _, cls := range p.SortedClasses() {
			fmt.Fprintf(&b, "       class: %s\n", cls)
		}
		for _, ex := range p.Examples {
			fmt.Fprintf(&b, "       e.g. %s\n", ex)
		}
	}
	b.WriteString("\nNothing has been changed. Apply selected entries with:\n  icebath approvals suggest --apply 1,3\nEntries are merged into command_allowlist in ~/.icebath/config.yaml.\n")
	return b.String()
}

// ProposalJSON is the machine readable shape of one proposal
type ProposalJSON struct {
	N        int      `json:"n"`
	Pattern  string   `json:"pattern"`
	Kind     string   `json:"kind"`
	Count    int      `json:"count"`
	Classes  []string `json:"classes"`
	Examples []string `json:"examples"`
}

// ProposalsJSON numbers proposals for JSON output
func ProposalsJSON(proposals []*Proposal) []ProposalJSON {
	out := make([]ProposalJSON, 0, len(proposals))
	for i, p := range proposals {
		examples := p.Examples
		if examples == nil {
			examples = []string{}
		}
		out = append(out, ProposalJSON{N: i + 1, Pattern: p.Pattern, Kind: p.Kind, Count: p.Count, Classes: p.SortedClasses(), Examples: examples})
	}
	return out
}
