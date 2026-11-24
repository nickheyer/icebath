// Command docgen writes cli-config.yaml.example and SETTINGS.md from the settings registry
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/settings"
)

func main() {
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	rows := settings.Rows(settings.DefaultsOnly(context.Background()), settings.Query{Advanced: true})
	example, err := exampleFile(rows)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*root, "cli-config.yaml.example"), example, 0o644); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*root, "SETTINGS.md"), reference(rows), 0o644); err != nil {
		log.Fatal(err)
	}
}

// Rows of each group in screen order
func byGroup(rows []settings.Row) map[settings.GroupID][]settings.Row {
	out := map[settings.GroupID][]settings.Row{}
	for _, r := range rows {
		out[r.Group] = append(out[r.Group], r)
	}
	return out
}

// Words of a row's choices
func choiceWords(r settings.Row) string {
	words := make([]string, len(r.Choices))
	for i, c := range r.Choices {
		words[i] = c.Value
	}
	return strings.Join(words, ", ")
}

// When a change takes effect, empty when at once
func applyNote(r settings.Row) string {
	switch r.Apply {
	case "next_turn":
		return "Applies from the next turn."
	case "new_session":
		return "Applies to new sessions."
	case "restart":
		return "Restart the background service to apply."
	}
	return ""
}

// One line of what a row does: short, help, choices and when it applies
func description(r settings.Row) []string {
	lines := []string{r.Label + ": " + r.Short}
	if r.Help != "" {
		lines = append(lines, r.Help)
	}
	if len(r.Choices) > 0 {
		lines = append(lines, "One of: "+choiceWords(r)+".")
	}
	if r.Null != "" && r.Default == nil {
		lines = append(lines, "Unset: "+r.Null+".")
	}
	if note := applyNote(r); note != "" {
		lines = append(lines, note)
	}
	switch {
	case r.Env != "" && r.EnvInvert:
		lines = append(lines, "The "+r.Env+" environment variable, when on, turns it off.")
	case r.Env != "":
		lines = append(lines, "The "+r.Env+" environment variable overrides it.")
	}
	return lines
}

// Nested YAML of one key at a value, ready to paste
func snippet(key string, value any) (string, error) {
	doc := map[string]any{}
	if err := config.Tree(doc).Set(key, value); err != nil {
		return "", err
	}
	out, err := config.SafeDump(doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// The example config: the schema version, then every setting commented out at its default
func exampleFile(rows []settings.Row) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# icebath settings, every one at its default.\n")
	b.WriteString("# Copy a setting into config.yaml without its leading \"# \", change it in the terminal UI with\n")
	b.WriteString("# /settings, or run: icebath config set <key> <value>\n")
	b.WriteString("# Generated from pkg/settings by go generate ./pkg/settings; edits here are overwritten.\n\n")
	grouped := byGroup(rows)
	for _, g := range settings.Groups() {
		fmt.Fprintf(&b, "\n# ============================================================\n# %s: %s\n# ============================================================\n", g.Label, g.Short)
		for _, r := range grouped[g.ID] {
			b.WriteString("\n")
			for _, line := range description(r) {
				b.WriteString("# " + line + "\n")
			}
			value := r.Default
			if r.Kind == settings.KindSecret {
				fmt.Fprintf(&b, "# Kept in .env, never in config.yaml.\n")
				continue
			}
			text, err := snippet(r.Key, value)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", r.Key, err)
			}
			for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
				b.WriteString("# " + line + "\n")
			}
		}
	}
	return b.Bytes(), nil
}

// Markdown text of a default value
func defaultText(r settings.Row) string {
	if r.Kind == settings.KindSecret {
		return "secret, in .env"
	}
	if r.Default == nil {
		if r.Null != "" {
			return "unset (" + r.Null + ")"
		}
		return "unset"
	}
	text, err := config.SafeDump(r.Default)
	if err != nil {
		return fmt.Sprint(r.Default)
	}
	s := strings.TrimSpace(string(text))
	if strings.Contains(s, "\n") || len(s) > 40 {
		return "see config"
	}
	return "`" + strings.ReplaceAll(s, "|", "\\|") + "`"
}

// The settings reference: each group with its everyday settings first, then the advanced ones
func reference(rows []settings.Row) []byte {
	var b bytes.Buffer
	b.WriteString("# Settings reference\n\n")
	b.WriteString("Every icebath setting, grouped as in the terminal UI's /settings screen. Change one there, with `icebath config set <key> <value>`, or in `config.yaml`.\n\n")
	b.WriteString("Generated from `pkg/settings` by `go generate ./pkg/settings`; edits here are overwritten.\n")
	grouped := byGroup(rows)
	for _, g := range settings.Groups() {
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", g.Label, g.Short)
		for _, advanced := range []bool{false, true} {
			var part []settings.Row
			for _, r := range grouped[g.ID] {
				if r.Advanced == advanced {
					part = append(part, r)
				}
			}
			if len(part) == 0 {
				continue
			}
			if advanced {
				b.WriteString("\n### Advanced\n")
			}
			b.WriteString("\n| Setting | Key | Default | What it does |\n|---|---|---|---|\n")
			for _, r := range part {
				what := strings.Join(description(r)[0:], " ")
				what = strings.TrimPrefix(what, r.Label+": ")
				fmt.Fprintf(&b, "| %s | `%s` | %s | %s |\n", r.Label, r.Key, defaultText(r), strings.ReplaceAll(what, "|", "\\|"))
			}
		}
	}
	return b.Bytes()
}
