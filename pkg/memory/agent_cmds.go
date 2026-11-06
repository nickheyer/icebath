package memory

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Clears memory.provider in config.yaml so only the built-in memory stays active
func MemoryOff(ctx context.Context, out io.Writer) error {
	h := home.Dir(ctx)
	tree, err := config.Load(h, configLookup(&tools.Env{Home: h}))
	if err != nil {
		return err
	}
	cfg := map[string]any(tree.Clone())
	mem, ok := cfg["memory"].(map[string]any)
	if !ok {
		mem = map[string]any{}
		cfg["memory"] = mem
	}
	mem["provider"] = ""
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		return err
	}
	fmt.Fprint(out, "\n  ✓ Memory provider: built-in only\n")
	fmt.Fprint(out, "  Saved to config.yaml\n\n")
	return nil
}

// Deletes MEMORY.md, USER.md or both from the home memories dir after a typed yes unless yes is set
func MemoryReset(ctx context.Context, out io.Writer, in io.Reader, target string, yes bool) error {
	h := home.Dir(ctx)
	memDir := filepath.Join(h, "memories")
	shown := home.Display(h)
	var files [][2]string
	if target == "all" || target == "memory" {
		files = append(files, [2]string{"MEMORY.md", "agent notes"})
	}
	if target == "all" || target == "user" {
		files = append(files, [2]string{"USER.md", "user profile"})
	}
	var existing [][2]string
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(memDir, f[0])); err == nil {
			existing = append(existing, f)
		}
	}
	if len(existing) == 0 {
		fmt.Fprintf(out, "\n  Nothing to reset — no memory files found in %s/memories/\n\n", shown)
		return nil
	}
	fmt.Fprint(out, "\n  This will permanently erase the following memory files:\n")
	for _, f := range existing {
		st, err := os.Stat(filepath.Join(memDir, f[0]))
		if err != nil {
			return err
		}
		size, err := pyrepr.Format(st.Size(), ",")
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "    ◆ %s (%s) — %s bytes\n", f[0], f[1], size)
	}
	if !yes {
		fmt.Fprint(out, "\n  Type 'yes' to confirm: ")
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			fmt.Fprint(out, "\n  Cancelled.\n\n")
			return nil
		}
		if strings.ToLower(strings.TrimSpace(strings.TrimSuffix(line, "\n"))) != "yes" {
			fmt.Fprint(out, "  Cancelled.\n\n")
			return nil
		}
	}
	for _, f := range existing {
		if err := os.Remove(filepath.Join(memDir, f[0])); err != nil {
			return err
		}
		fmt.Fprintf(out, "  ✓ Deleted %s (%s)\n", f[0], f[1])
	}
	fmt.Fprint(out, "\n  Memory reset complete. New sessions will start with a blank slate.\n")
	fmt.Fprintf(out, "  Files were in: %s/memories/\n\n", shown)
	return nil
}

// Prints the terminal insights report for the home state.db, the no data line when absent and the error line on failure
func CmdInsights(ctx context.Context, out io.Writer, days int, source string) {
	path := home.StatePath(home.Dir(ctx))
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(out, "  %s\n", i18n.T(ctx, "cli.insights.no_session_data", "", nil))
		return
	}
	text, err := insightsTerminalReport(ctx, path, days, source)
	if err != nil {
		fmt.Fprintln(out, i18n.T(ctx, "gateway.insights.error", "", map[string]any{"error": err.Error()}))
		return
	}
	fmt.Fprintln(out, text)
}

// Opens the session store read only, renders the report and always closes the store
func insightsTerminalReport(ctx context.Context, path string, days int, source string) (string, error) {
	db, err := state.Open(path, state.Options{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	engine, err := OpenInsightsEngine(db)
	if err != nil {
		return "", err
	}
	defer func() { _ = engine.Close() }()
	report, err := engine.Generate(ctx, days, source)
	if err != nil {
		return "", err
	}
	return engine.FormatTerminal(report), nil
}
