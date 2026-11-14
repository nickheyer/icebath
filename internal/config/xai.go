package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// Page of the xAI guide for the models it retired
const XAIMigrationGuideURL = "https://docs.x.ai/developers/migration/may-15-retirement"

// Day xAI retired the models the migration rewrites
const XAIRetirementDate = "May 15, 2026"

// Successor of one retired xAI model with the reasoning effort that keeps its behavior
type xaiSuccessor struct {
	model  string
	effort string
}

// Retired xAI model ids with their successors from the xAI migration guide
func retiredXAIModels() map[string]xaiSuccessor {
	return map[string]xaiSuccessor{
		"grok-4-0709":                 {model: "grok-4.3"},
		"grok-4-fast-reasoning":       {model: "grok-4.3"},
		"grok-4-fast-non-reasoning":   {model: "grok-4.3", effort: "none"},
		"grok-4-1-fast-reasoning":     {model: "grok-4.3"},
		"grok-4-1-fast-non-reasoning": {model: "grok-4.3", effort: "none"},
		"grok-code-fast-1":            {model: "grok-4.3"},
		"grok-3":                      {model: "grok-4.3"},
		"grok-imagine-image-pro":      {model: "grok-imagine-image-quality"},
	}
}

// Config slot naming a retired xAI model, the model kept exactly as written
type XAIRetirement struct {
	ConfigPath      string
	CurrentModel    string
	Replacement     string
	ReasoningEffort string
}

// One line naming the slot, its model, the successor and the reasoning effort to set
func (r XAIRetirement) String() string {
	text := fmt.Sprintf("%s: %s → use %s", r.ConfigPath, quoted(r.CurrentModel), quoted(r.Replacement))
	if r.ReasoningEffort != "" {
		text += fmt.Sprintf(` (set reasoning_effort: "%s")`, r.ReasoningEffort)
	}
	return text
}

// Lower case xAI model id without its provider prefix
func xaiModelID(model string) string {
	id := strings.ToLower(trimEnvSpace(model))
	for _, prefix := range []string{"x-ai/", "xai/"} {
		if rest, ok := strings.CutPrefix(id, prefix); ok {
			return rest
		}
	}
	return id
}

// Retired xAI models named by the model slots of a config tree, in slot order
func findRetiredXAI(config *yaml.Node) []XAIRetirement {
	var out []XAIRetirement
	check := func(path string, model *yaml.Node) {
		if !isStringNode(model) {
			return
		}
		written := displayText(model)
		id := xaiModelID(written)
		successor, retired := retiredXAIModels()[id]
		if !strings.HasPrefix(id, "grok-") || !retired {
			return
		}
		out = append(out, XAIRetirement{ConfigPath: path, CurrentModel: written, Replacement: successor.model, ReasoningEffort: successor.effort})
	}
	section := func(path ...string) {
		if sec := treeGet(config, path...); isMapping(sec) {
			check(strings.Join(path, ".")+".model", childOf(sec, "model"))
		}
	}
	section("principal")
	if aux := childOf(config, "auxiliary"); isMapping(aux) {
		for i := 0; i+1 < len(aux.Content); i += 2 {
			if slot := aux.Content[i+1]; isMapping(slot) {
				check("auxiliary."+displayText(aux.Content[i])+".model", childOf(slot, "model"))
			}
		}
	}
	section("delegation")
	section("tts", "xai")
	section("plugins", "image_gen", "xai")
	return out
}

// Retired xAI models the loaded config of ctx still names
func (s *Store) RetiredXAIModels(ctx context.Context) ([]XAIRetirement, error) {
	cfg, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	tree, err := encodeValue(reflect.ValueOf(cfg))
	if err != nil {
		return nil, err
	}
	return findRetiredXAI(plainCopy(tree)), nil
}

// Outcome of rewriting the retired xAI model slots of the config file, Backup empty when no copy was made
type XAIMigrationResult struct {
	Path     string
	Backup   string
	Resolved []XAIRetirement
	Changed  bool
}

// Config slot whose parent holds a value that takes no keys
type XAISlotError struct {
	ConfigPath string
}

// The slot that cannot be rewritten
func (e *XAISlotError) Error() string {
	return fmt.Sprintf("cannot rewrite %s: its parent is not a mapping", quoted(e.ConfigPath))
}

// Rewrites the slots of issues in the config file atomically, comments and literals kept, after a backup when asked
func (s *Store) ApplyXAIMigration(ctx context.Context, issues []XAIRetirement, backup bool) (*XAIMigrationResult, error) {
	if err := s.RequireFileTooling(ctx, "The xAI model migration"); err != nil {
		return nil, err
	}
	_, path, err := s.configPath(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	result := &XAIMigrationResult{Path: path}
	if len(issues) == 0 {
		return result, nil
	}
	locked, release, err := s.lockWrite(ctx, path)
	if err != nil {
		return nil, err
	}
	err = s.rewriteXAISlots(locked, path, issues, backup, result)
	if err = errors.Join(err, release()); err != nil {
		return nil, err
	}
	return result, nil
}

// Rewrite of the slots with the write lock held, filling result
func (s *Store) rewriteXAISlots(ctx context.Context, path string, issues []XAIRetirement, backup bool, result *XAIMigrationResult) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	current, err := ParseYAMLDocument(data)
	if err != nil {
		return err
	}
	if current.root == nil || isNull(current.root) {
		return nil
	}
	root := plainCopy(current.root)
	var resolved []XAIRetirement
	for _, issue := range issues {
		parts := strings.Split(issue.ConfigPath, ".")
		if len(parts) < 2 {
			return fmt.Errorf("Path must have at least one parent: %s", quoted(issue.ConfigPath))
		}
		parent, found := root, true
		for _, segment := range parts[:len(parts)-1] {
			at := mappingIndex(parent, segment)
			if at < 0 {
				found = false
				break
			}
			parent = parent.Content[at]
		}
		if !found {
			continue
		}
		if !isMapping(parent) {
			return &XAISlotError{ConfigPath: issue.ConfigPath}
		}
		putKey(parent, parts[len(parts)-1], stringNode(issue.Replacement))
		if issue.ReasoningEffort != "" {
			putKey(parent, "reasoning_effort", stringNode(issue.ReasoningEffort))
		}
		resolved = append(resolved, issue)
	}
	if len(resolved) == 0 {
		return nil
	}
	if backup {
		result.Backup, _ = s.backupConfig(ctx, path, "pre-migrate-xai")
	}
	if _, err := s.requireReadable(ctx, path); err != nil {
		return err
	}
	out, err := current.withRoot(root).Render()
	if err != nil {
		return err
	}
	if err := writeConfigText(ctx, path, out); err != nil {
		return err
	}
	result.Resolved, result.Changed = resolved, true
	return nil
}

// What the xAI migration command shows and whether it ends with a failing status
type XAIMigrationReport struct {
	Lines  []Line
	Failed bool
}

// Line on stderr under a red cross that ends the command with a failing status
func xaiFailure(message string) Line {
	return Line{Spans: []Span{{Text: "  "}, {Text: "✗", Tone: ToneError}, {Text: " " + message}}, Stderr: true}
}

// Line led by a toned mark at an indent
func markedLine(indent string, tone Tone, mark, text string) Line {
	return Line{Spans: []Span{{Text: indent}, {Text: mark, Tone: tone}, {Text: " " + text}}}
}

// Runs the xAI model migration command, a dry run unless apply is set, a backup made unless declined
func (s *Store) MigrateXAI(ctx context.Context, apply, backup bool) (*XAIMigrationReport, error) {
	issues, err := s.RetiredXAIModels(ctx)
	if err != nil {
		return nil, err
	}
	report := &XAIMigrationReport{Lines: []Line{blankLine(),
		tonedLine(ToneTitle, fmt.Sprintf("◆ xAI Model Retirement Migration (%s)", XAIRetirementDate)), blankLine()}}
	if len(issues) == 0 {
		report.Lines = append(report.Lines, markedLine("  ", ToneSuccess, "✓", "No retired xAI models in config — nothing to migrate."))
		return report, nil
	}
	report.Lines = append(report.Lines, plainLine(fmt.Sprintf("  Found %d retired xAI model reference(s):", len(issues))), blankLine())
	for _, issue := range issues {
		report.Lines = append(report.Lines, markedLine("    ", ToneWarning, "⚠", issue.String()))
	}
	report.Lines = append(report.Lines, blankLine(), markedLine("    ", ToneFrame, "→", "Migration guide: "+XAIMigrationGuideURL), blankLine())
	_, path, err := s.configPath(ctx)
	if err != nil {
		return nil, err
	}
	if !apply {
		report.Lines = append(report.Lines, tonedLine(ToneDim, "Dry-run mode — no changes written."),
			tonedLine(ToneDim, fmt.Sprintf("Re-run with `icebath migrate xai --apply` to rewrite %s in-place (backup created automatically).", path)))
		return report, nil
	}
	b, bh, err := s.route(ctx, path)
	if err != nil {
		return nil, err
	}
	exists, err := b.Exists(ctx, bh)
	if err != nil {
		return nil, err
	}
	if !exists {
		report.Lines, report.Failed = append(report.Lines, xaiFailure(fmt.Sprintf("Could not locate config.yaml (looked at: %s)", path))), true
		return report, nil
	}
	result, err := s.ApplyXAIMigration(ctx, issues, backup)
	if err != nil {
		report.Lines, report.Failed = append(report.Lines, xaiFailure(fmt.Sprintf("Migration failed: %v", err))), true
		return report, nil
	}
	if !result.Changed {
		report.Lines = append(report.Lines, markedLine("  ", ToneWarning, "⚠", "No changes written."))
		return report, nil
	}
	if result.Backup != "" {
		report.Lines = append(report.Lines, markedLine("  ", ToneSuccess, "✓", "Backup: "+result.Backup))
	}
	report.Lines = append(report.Lines, markedLine("  ", ToneSuccess, "✓", fmt.Sprintf("Updated %d slot(s) in %s", len(result.Resolved), result.Path)),
		blankLine(), tonedLine(ToneDim, "Run `icebath doctor` to confirm no retired xAI models remain."))
	return report, nil
}
