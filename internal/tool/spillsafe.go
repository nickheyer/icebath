package tool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
)

const (
	hookSpillMaxChars    = 10_000
	hookSpillPreviewHead = 500
	hookSpillPreviewTail = 500
)

type SpillDirError struct {
	Path string
}

func (e *SpillDirError) Error() string {
	return "spill dir is not a directory (symlink?): " + e.Path
}

type OverwriteDirError struct {
	Path string
}

func (e *OverwriteDirError) Error() string {
	return "refusing to overwrite a directory: " + e.Path
}

func EnsureSpillDir(ctx context.Context, homes *home.Resolver, dir string, private bool) error {
	if homes == nil {
		return &UnfilledError{Name: "home.Resolver"}
	}
	perm := fs.FileMode(0o777)
	if private {
		perm = 0o700
	}
	if err := homes.MakeUserDir(ctx, dir, perm, true); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return &SpillDirError{Path: dir}
	}
	if private && info.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

func openExclusive(p string, private, overwrite bool) (*os.File, error) {
	if overwrite {
		info, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, err
		case info.IsDir():
			return nil, &OverwriteDirError{Path: p}
		default:
			if err := os.Remove(p); err != nil {
				return nil, err
			}
		}
	}
	perm := fs.FileMode(0o666)
	if private {
		perm = 0o600
	}
	return os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
}

func WriteExclusive(p, text string, private, overwrite bool) error {
	f, err := openExclusive(p, private, overwrite)
	if err != nil {
		return err
	}
	if !utf8.ValidString(text) {
		f.Close()
		return fmt.Errorf("write %s: text is not valid UTF-8", p)
	}
	if textNewline != "\n" {
		text = strings.ReplaceAll(text, "\n", textNewline)
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

type HookSpillConfig struct {
	Enabled     bool
	MaxChars    int
	PreviewHead int
	PreviewTail int
	Directory   string
}

func HookSpillSettings(cfg *config.Config) HookSpillConfig {
	c := HookSpillConfig{Enabled: true, MaxChars: hookSpillMaxChars, PreviewHead: hookSpillPreviewHead, PreviewTail: hookSpillPreviewTail}
	if cfg == nil || cfg.Hooks.OutputSpill == nil {
		return c
	}
	s := cfg.Hooks.OutputSpill
	if s.Enabled != nil {
		c.Enabled = *s.Enabled
	}
	if s.MaxChars != nil && *s.MaxChars >= 1 {
		c.MaxChars = *s.MaxChars
	}
	if s.PreviewHead != nil && *s.PreviewHead >= 0 {
		c.PreviewHead = *s.PreviewHead
	}
	if s.PreviewTail != nil && *s.PreviewTail >= 0 {
		c.PreviewTail = *s.PreviewTail
	}
	c.Directory = s.Directory
	return c
}

func hookSpillDir(homeDir, override, sessionID string) string {
	base := home.HookOutputs.In(homeDir)
	if override != "" {
		base = home.ExpandUser(override)
	}
	segment := sessionID
	if segment == "" {
		segment = "no-session"
	}
	segment = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(segment, "/", "_"), "\\", "_"), "..", "_")
	return filepath.Join(base, segment)
}

func randomUUIDHex() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:]), nil
}

func SpillHookOutput(ctx context.Context, homes *home.Resolver, homeDir, text, sessionID, source string, c HookSpillConfig) string {
	if !c.Enabled {
		return text
	}
	maxChars := c.MaxChars
	if maxChars <= 0 {
		maxChars = hookSpillMaxChars
	}
	runes := []rune(text)
	total := len(runes)
	if total <= maxChars {
		return text
	}
	if source == "" {
		source = "hook"
	}
	saved, err := writeHookSpill(ctx, homes, hookSpillDir(homeDir, c.Directory, sessionID), text)
	if err != nil {
		slog.Warn(fmt.Sprintf("hook output spill failed: %v", err))
	}
	head := "[" + source + " output truncated — " + groupThousands(total) + " chars; full content "
	if saved != "" {
		head += "saved to " + saved + "]"
	} else {
		head += "unavailable — spill write failed]"
	}
	parts := []string{head}
	if c.PreviewHead > 0 {
		if h := string(runes[:min(c.PreviewHead, total)]); h != "" {
			parts = append(parts, "--- head ---", h)
		}
	}
	if c.PreviewTail > 0 && total > c.PreviewHead {
		parts = append(parts, "--- tail ---", string(runes[total-min(c.PreviewTail, total):]))
	}
	return strings.Join(parts, "\n")
}

func writeHookSpill(ctx context.Context, homes *home.Resolver, dir, text string) (string, error) {
	if err := EnsureSpillDir(ctx, homes, dir, true); err != nil {
		return "", err
	}
	name, err := randomUUIDHex()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, name+".txt")
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if err := WriteExclusive(p, text, true, false); err != nil {
		return "", err
	}
	return p, nil
}
