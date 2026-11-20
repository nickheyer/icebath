package provider

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"gopkg.in/yaml.v3"
)

const (
	XAIMigrationGuideURL = "https://docs.x.ai/developers/migration/may-15-retirement"
	XAIRetirementDate    = "May 15, 2026"
)

// xaiRetiredModel is the official replacement for one retired xAI model
type xaiRetiredModel struct {
	Replacement     string
	ReasoningEffort string
	Note            string
}

// Retired xAI models and their replacements per the xAI migration guide
var xaiRetiredModels = map[string]xaiRetiredModel{
	"grok-4-0709":                 {Replacement: "grok-4.3"},
	"grok-4-fast-reasoning":       {Replacement: "grok-4.3"},
	"grok-4-fast-non-reasoning":   {Replacement: "grok-4.3", ReasoningEffort: "none"},
	"grok-4-1-fast-reasoning":     {Replacement: "grok-4.3"},
	"grok-4-1-fast-non-reasoning": {Replacement: "grok-4.3", ReasoningEffort: "none"},
	"grok-code-fast-1":            {Replacement: "grok-4.3"},
	"grok-3":                      {Replacement: "grok-4.3"},
	"grok-imagine-image-pro":      {Replacement: "grok-imagine-image-quality"},
}

// RetirementIssue is a reference to a retired xAI model found in an icebath config
type RetirementIssue struct {
	ConfigPath      string
	CurrentModel    string
	Replacement     string
	ReasoningEffort string
	Note            string
}

// Lowercased model id with any x-ai or xai provider prefix removed
func xaiRetirementNormalize(modelID string) string {
	m := strings.ToLower(strings.TrimSpace(modelID))
	for _, prefix := range []string{"x-ai/", "xai/"} {
		if strings.HasPrefix(m, prefix) {
			return m[len(prefix):]
		}
	}
	return m
}

func xaiRetirementLooksLikeXAI(model any) bool {
	s, ok := model.(string)
	return ok && strings.HasPrefix(xaiRetirementNormalize(s), "grok-")
}

// FindRetiredXAIRefs lists retired xAI models in the principal, auxiliary, delegation, tts and image gen slots
func FindRetiredXAIRefs(cfg any) []RetirementIssue {
	issues := []RetirementIssue{}
	root, ok := cfg.(map[string]any)
	if !ok {
		return issues
	}
	check := func(path string, model any) {
		if !xaiRetirementLooksLikeXAI(model) {
			return
		}
		name := model.(string)
		if entry, ok := xaiRetiredModels[xaiRetirementNormalize(name)]; ok {
			issues = append(issues, RetirementIssue{ConfigPath: path, CurrentModel: name, Replacement: entry.Replacement,
				ReasoningEffort: entry.ReasoningEffort, Note: entry.Note})
		}
	}
	sectionAt := func(keys ...string) map[string]any {
		var node any = root
		for _, key := range keys {
			m, ok := node.(map[string]any)
			if !ok {
				return nil
			}
			node = m[key]
		}
		m, _ := node.(map[string]any)
		return m
	}
	checkSection := func(path ...string) {
		if section := sectionAt(path...); section != nil {
			check(strings.Join(path, ".")+".model", section["model"])
		}
	}
	checkSection("principal")
	aux := sectionAt("auxiliary")
	slots := make([]string, 0, len(aux))
	for name := range aux {
		slots = append(slots, name)
	}
	sort.Strings(slots)
	for _, name := range slots {
		if slot, ok := aux[name].(map[string]any); ok {
			check("auxiliary."+name+".model", slot["model"])
		}
	}
	for _, path := range [][]string{{"delegation"}, {"tts", "xai"}, {"plugins", "image_gen", "xai"}} {
		checkSection(path...)
	}
	return issues
}

// FormatRetirementIssue renders one retirement issue as a single human readable line
func FormatRetirementIssue(issue RetirementIssue) string {
	parts := []string{fmt.Sprintf("%s: %s → use %s", issue.ConfigPath, pyStringRepr(issue.CurrentModel), pyStringRepr(issue.Replacement))}
	if issue.ReasoningEffort != "" {
		parts = append(parts, fmt.Sprintf("(set reasoning_effort: %q)", issue.ReasoningEffort))
	}
	if issue.Note != "" {
		parts = append(parts, fmt.Sprintf("[note: %s]", issue.Note))
	}
	return strings.Join(parts, " ")
}

// RetirementApplyResult is the outcome of rewriting a config for the xAI retirement
type RetirementApplyResult struct {
	FilePath       string
	BackupPath     string
	IssuesResolved []RetirementIssue
	ConfigChanged  bool
}

// Parent mapping and leaf key of a dotted slot path, false when a segment is missing
func xaiWalkToParent(doc any, dottedPath string) (map[string]any, string, bool, error) {
	segments := strings.Split(dottedPath, ".")
	parents, leaf := segments[:len(segments)-1], segments[len(segments)-1]
	if len(parents) == 0 {
		return nil, "", false, fmt.Errorf("Path must have at least one parent: %s", pyStringRepr(dottedPath))
	}
	node := doc
	for _, segment := range parents {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, "", false, nil
		}
		next, has := m[segment]
		if !has {
			return nil, "", false, nil
		}
		node = next
	}
	parent, ok := node.(map[string]any)
	if !ok {
		return nil, "", false, fmt.Errorf("Path parent of %s is not a mapping", pyStringRepr(dottedPath))
	}
	return parent, leaf, true, nil
}

// ApplyXAIRetirementMigration rewrites the config file in place keeping comments, order and literals, backing it up first
func ApplyXAIRetirementMigration(ctx context.Context, configPath string, issues []RetirementIssue, backup bool) (RetirementApplyResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := config.RequireFileTooling(ctx, "The xAI model migration"); err != nil {
		return RetirementApplyResult{}, err
	}
	if _, err := os.Stat(configPath); err != nil {
		return RetirementApplyResult{}, err
	}
	unchanged := RetirementApplyResult{FilePath: configPath, IssuesResolved: []RetirementIssue{}}
	if len(issues) == 0 {
		return unchanged, nil
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return RetirementApplyResult{}, err
	}
	var doc any
	if err := yaml.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &doc); err != nil {
		return RetirementApplyResult{}, err
	}
	if doc == nil {
		return unchanged, nil
	}
	resolved := []RetirementIssue{}
	for _, issue := range issues {
		parent, leaf, ok, err := xaiWalkToParent(doc, issue.ConfigPath)
		if err != nil {
			return RetirementApplyResult{}, err
		}
		if !ok {
			continue
		}
		parent[leaf] = issue.Replacement
		if issue.ReasoningEffort != "" {
			parent["reasoning_effort"] = issue.ReasoningEffort
		}
		resolved = append(resolved, issue)
	}
	if len(resolved) == 0 {
		return unchanged, nil
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return RetirementApplyResult{}, fmt.Errorf("top-level YAML must be a mapping")
	}
	backupPath := ""
	if backup {
		var err error
		if backupPath, err = config.BackupConfig(ctx, configPath, "pre-migrate-xai", config.DefaultBackupKeep); err != nil {
			return RetirementApplyResult{}, err
		}
	}
	if _, err := config.RequireReadableConfigBeforeWrite(ctx, configPath); err != nil {
		return RetirementApplyResult{}, err
	}
	if err := config.WriteConfigDocument(ctx, configPath, root, ""); err != nil {
		return RetirementApplyResult{}, err
	}
	return RetirementApplyResult{FilePath: configPath, BackupPath: backupPath, IssuesResolved: resolved, ConfigChanged: true}, nil
}
