package hooks

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	hooksconfig "github.com/nickheyer/icebath/internal/hooks/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Characters above which hook output is saved when the setting holds no usable number
const spillDefault = 10000

// Text a hook hands the turn, saved to a file of the session's under the hook output folder when it is longer than the profile allows and
// a preview naming the file kept in its place: so many characters of its start and end, source naming what gave it
func Spill(cfg *config.Config, text, session, source string) string {
	if !cfg.GetBool(hooksconfig.SpillEnabled) {
		return text
	}
	limit := cfg.GetInt(hooksconfig.SpillMaxChars)
	if limit <= 0 {
		limit = spillDefault
	}
	total := utf8.RuneCountInString(text)
	if total <= limit {
		return text
	}
	head, tail := max(cast.ToInt(cfg.Get(hooksconfig.SpillPreviewHead)), 0), max(cast.ToInt(cfg.Get(hooksconfig.SpillPreviewTail)), 0)
	saved, err := spill(cfg, text, session)
	if err != nil {
		slog.Warn(fmt.Sprintf("hook output spill failed: %v", err))
	}
	first := fmt.Sprintf("[%s output truncated — %s chars; full content ", source, strs.Thousands(total))
	if saved != "" {
		first += "saved to " + saved + "]"
	} else {
		first += "unavailable — spill write failed]"
	}
	parts := []string{first}
	if head > 0 {
		parts = append(parts, "--- head ---", strs.Head(text, head))
	}
	if tail > 0 && total > head {
		parts = append(parts, "--- tail ---", strs.Tail(text, tail))
	}
	return strings.Join(parts, "\n")
}

// Writes the text to a new file of the session's folder of hook output, the folder private and never a link
func spill(cfg *config.Config, text, session string) (string, error) {
	base := filepath.Join(cfg.ProfileDir(cfg.Profile()), "hook_outputs")
	if dir := strings.TrimSpace(cfg.GetString(hooksconfig.SpillDirectory)); dir != "" {
		base = fsx.ExpandHome(dir)
	}
	segment := strings.NewReplacer("/", "_", `\`, "_", "..", "_").Replace(session)
	if segment == "" {
		segment = "no-session"
	}
	dir := filepath.Join(base, segment)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("spill dir is not a directory (symlink?): %s", dir)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
	}
	path := filepath.Join(dir, strs.Random(16)+".txt")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if _, err := file.WriteString(text); err != nil {
		file.Close()
		os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}
