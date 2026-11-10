package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Glyph and trailing note of each context file status, an empty note meaning none
var contextFileStatusDisplay = map[string][2]string{
	"loaded":     {"✓", ""},
	"truncated":  {"◐", "truncated — over context_file_max_chars"},
	"shadowed":   {"○", "not loaded — higher-priority context type wins"},
	"blocked":    {"✗", "not loaded — blocked by the prompt-injection scan"},
	"flagged":    {"⚠", "loaded — matched prompt-injection pattern(s); review the file"},
	"empty":      {"○", "not loaded — empty file"},
	"unreadable": {"✗", "not loaded — could not be read"},
	"suppressed": {"○", "not loaded — cwd fell back to the Icebath install tree"},
}

// ContextFileSource is one context file the prompt builder considered with its size and load status
type ContextFileSource struct {
	Label     string `json:"label"`
	Path      string `json:"path"`
	Chars     int    `json:"chars"`
	EstTokens int    `json:"est_tokens"`
	Loaded    bool   `json:"loaded"`
	Status    string `json:"status"`
}

// ContextFileSourcesOptions mirrors the context files prompt arguments, a nil Cwd meaning the launch directory
type ContextFileSourcesOptions struct {
	Cwd                      *string
	ContextLength            *int
	AllowInstallTreeFallback bool
	HomeOverride             string
	SkipSoul                 bool
}

// Manifest row of one file with its code point size and rough token estimate
func contextFileEntry(label, path, content, status string) ContextFileSource {
	return ContextFileSource{Label: label, Path: path, Chars: utf8.RuneCountInString(content), EstTokens: provider.EstimateTokens(content),
		Loaded: status == "loaded" || status == "truncated" || status == "flagged", Status: status}
}

// Unreadable when a file the builder read as empty still has bytes or cannot be stat'ed, else empty
func contextFileEmptyStatus(path string) string {
	st, err := os.Stat(path)
	if err != nil || st.Size() > 0 {
		return "unreadable"
	}
	return "empty"
}

// Flagged or blocked on a threat scan hit, truncated past the cap, else loaded
func contextFileLoadedStatus(content string, renderedLen, maxChars int, userAuthored bool) string {
	if hits, _ := tools.ScanForThreats(strings.TrimLeft(content, "\ufeff"), "context"); len(hits) > 0 {
		if userAuthored {
			return "flagged"
		}
		return "blocked"
	}
	if renderedLen > maxChars {
		return "truncated"
	}
	return "loaded"
}

// One row per context file considered in the builder's priority order, the profile SOUL.md last
func ListContextFileSources(ctx context.Context, env *tools.Env, o ContextFileSourcesOptions) []ContextFileSource {
	cwd := ""
	if o.Cwd != nil {
		cwd = *o.Cwd
	} else {
		cwd, _ = os.Getwd()
	}
	cwdPath := resolvePath(cwd)
	maxChars := tools.ContextFileMaxCharsFor(env, o.ContextLength)
	suppressed := projectContextSuppressed(o.Cwd, cwdPath, o.AllowInstallTreeFallback)
	sources := []ContextFileSource{}
	winner := ""
	for _, f := range DiscoverContextFiles(env, cwdPath) {
		var status string
		switch {
		case f.Content == "":
			status = contextFileEmptyStatus(f.Path)
		case suppressed:
			status = "suppressed"
		case winner == "" || winner == f.Kind:
			winner = f.Kind
			status = contextFileLoadedStatus(f.Content, utf8.RuneCountInString("## "+f.Label+"\n\n"+f.Content), maxChars, false)
		default:
			status = "shadowed"
		}
		sources = append(sources, contextFileEntry(f.Label, f.Path, f.Content, status))
	}
	if !o.SkipSoul {
		h := o.HomeOverride
		if h == "" {
			h = home.Dir(ctx)
		}
		soulPath := filepath.Join(h, "SOUL.md")
		if existsOrDenied(soulPath) {
			content := readContextFile(env, soulPath)
			status := contextFileEmptyStatus(soulPath)
			if content != "" {
				status = contextFileLoadedStatus(content, utf8.RuneCountInString(content), maxChars, true)
			}
			sources = append(sources, contextFileEntry("SOUL.md", soulPath, content, status))
		}
	}
	return sources
}

// Manifest for a live agent resolved like its context files prompt tier, nothing when context files are skipped
func ContextFileSourcesForAgent(ctx context.Context, a *AIAgent) ([]ContextFileSource, error) {
	if a.skipContextFiles {
		return []ContextFileSource{}, nil
	}
	cwd, err := tools.ResolveContextCwd(a.env, !a.contextCwdIsLaunchArtifact)
	if err != nil {
		return nil, err
	}
	o := ContextFileSourcesOptions{AllowInstallTreeFallback: a.platform == "cli" || a.platform == "tui", HomeOverride: agentOwnHome(ctx, a)}
	if cwd != "" {
		o.Cwd = &cwd
	}
	if a.contextCompressor != nil {
		n := engineContextLength(a.contextCompressor)
		o.ContextLength = &n
	}
	return ListContextFileSources(ctx, a.env, o), nil
}

// Plain text context files block for the context command, nothing when no file was found
func RenderContextFileLines(sources []ContextFileSource) []string {
	if len(sources) == 0 {
		return []string{}
	}
	width := 0
	for _, src := range sources {
		width = max(width, utf8.RuneCountInString(src.Label))
	}
	lines := []string{"Context files"}
	for _, src := range sources {
		display, ok := contextFileStatusDisplay[src.Status]
		if !ok {
			display = [2]string{"•", ""}
		}
		suffix := ""
		if display[1] != "" {
			suffix = "  (" + display[1] + ")"
		}
		row, _ := i18n.PyFormat("{glyph} {label:<{width}} ~{tokens:>9,} tokens{suffix}",
			map[string]any{"glyph": display[0], "label": src.Label, "width": width, "tokens": src.EstTokens, "suffix": suffix})
		lines = append(lines, row)
	}
	return lines
}
