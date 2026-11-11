package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

var foreignSourceLabels = map[string]string{"claude": "Claude Code", "codex": "Codex CLI"}

// Imports the named or picked foreign session and returns its new session id, false when nothing was imported
func runSessionsImport(ctx context.Context, ns map[string]any, w io.Writer) (string, bool, error) {
	source, path := nsText(ns, "from_source"), nsText(ns, "path")
	if path != "" {
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintf(w, "Error: file not found: %s\n", path)
			return "", false, nil
		}
		if source == "" {
			if strings.Contains(path, "/.claude/") || strings.HasSuffix(path, ".jsonl") && strings.Contains(path, "claude") {
				source = "claude"
			}
			if strings.Contains(path, "/.codex/") || strings.HasPrefix(pypath.PathName(path), "rollout-") {
				source = "codex"
			}
		}
		if source == "" {
			fmt.Fprintln(w, "Cannot infer source from path; pass --from claude|codex.")
			return "", false, nil
		}
		path = pypath.PathStr(path)
	} else {
		picked, err := PickForeignSession(ctx, source, w)
		if err != nil {
			return "", false, err
		}
		if picked == nil {
			return "", false, nil
		}
		source, path = picked.Source, picked.Path
	}
	sessionID, err := ImportForeignSession(ctx, source, path)
	if isValueError(err) {
		fmt.Fprintf(w, "Error: %s\n", sessionsExcText(err))
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	label, ok := foreignSourceLabels[source]
	if !ok {
		label = source
	}
	fmt.Fprintf(w, "✓ Imported %s session as %s\n", label, sessionID)
	fmt.Fprintf(w, "  Continue it with:  icebath --resume %s\n", sessionID)
	return sessionID, true, nil
}
