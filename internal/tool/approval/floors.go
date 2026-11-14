package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
)

func (d *Detector) DenyRule(command string, globs []string) (string, bool) {
	var patterns []string
	for _, g := range globs {
		if g = pyStrip(g); g != "" {
			patterns = append(patterns, g)
		}
	}
	if len(patterns) == 0 {
		return "", false
	}
	lowered := make([]string, len(patterns))
	for k, p := range patterns {
		lowered[k] = pyLower(p)
	}
	match := ""
	d.denyVariants(command, func(v string) bool {
		candidate := pyStrip(pyLower(v))
		for k, p := range lowered {
			if globMatch(candidate, p) {
				match = patterns[k]
				return false
			}
		}
		return true
	})
	return match, match != ""
}

func DenyBlock(pattern string) tool.Decision {
	return tool.Decision{Message: "BLOCKED: this command matches the user-defined deny rule '" + pattern +
		"' (approvals.deny in config.yaml). It cannot be executed via the agent — not even with --yolo, /yolo, or " +
		"approvals.mode=off. Do NOT retry or rephrase this command; the user has explicitly forbidden it."}
}

func SudoBlock(description string) tool.Decision {
	return tool.Decision{Message: "BLOCKED: " + description + ". Do not pipe passwords to 'sudo -S' — this is a brute-force " +
		"attack vector. Set SUDO_PASSWORD in your .env file if the agent needs passwordless sudo, or run the sudo command " +
		"manually in your own terminal."}
}

const recoveryPrefix = " RECOVERY: this block fires on oversized/unparseable inline command payloads (heredocs, giant " +
	"one-liners), not on the operation itself. "

func HardlineBlock(ctx context.Context, profileHome, description, command string) tool.Decision {
	message := "BLOCKED (hardline): " + description + ". This command is on the unconditional blocklist and cannot be " +
		"executed via the agent — not even with --yolo, /yolo, approvals.mode=off, or cron approve mode. If you genuinely " +
		"need to run it, run it yourself in a terminal outside the agent."
	if description == parserLimitDesc || description == malformedExecDesc {
		saved := ""
		if command != "" {
			if path, err := saveBlockedPayload(ctx, profileHome, command); err != nil {
				slog.DebugContext(ctx, "failed to save blocked payload", "error", err)
			} else {
				saved = path
			}
		}
		if saved != "" {
			message += recoveryPrefix + "Your command was saved to " + saved + " — review it, then run: terminal(command=\"bash " +
				saved + "\"). Do not retry inline."
		} else {
			message += recoveryPrefix + "Write the script to a file with write_file, then run it: terminal(command=\"bash " +
				"/path/script.sh\") or \"python3 /path/script.py\". Do not retry inline."
		}
	}
	return tool.Decision{Message: message}
}

func saveBlockedPayload(ctx context.Context, profileHome, command string) (string, error) {
	if profileHome == "" {
		return "", fmt.Errorf("blocked payload needs the profile home")
	}
	dir := home.BlockedScripts.In(profileHome)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	if _, err := fileutil.RemoveOlderThan(dir, "blocked-*.sh", 7*24*time.Hour, time.Now()); err != nil {
		slog.DebugContext(ctx, "blocked payload sweep failed", "error", err)
	}
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("blocked-%d-%s.sh", time.Now().Unix(), hex.EncodeToString(nonce[:])))
	body := "#!/usr/bin/env bash\n" +
		"# Auto-saved by icebath: this command exceeded the inline command\n" +
		"# parser limit and was blocked from direct execution. Review it,\n" +
		"# then run it via: bash " + path + "\n" + command
	if !strings.HasSuffix(command, "\n") {
		body += "\n"
	}
	if err := fileutil.WriteText(ctx, path, replaceInvalid(body)); err != nil {
		return "", err
	}
	return path, nil
}

func replaceInvalid(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && w == 1 {
			b.WriteByte('?')
		} else {
			b.WriteString(s[i : i+w])
		}
		i += w
	}
	return b.String()
}

var reinterpretedArgumentRe = regexp.MustCompile(`(?:^|[ \t])(?:-[^-` + pySpaceClass + `]*[ce]|--(?:command|eval))(?:[= \t]|\n?\z)`)

const shellControlChars = "\n\r;&|<>`$()"

func AllowlistOperator(command string) bool {
	var quote byte
	reinterpretable := false
	for i := 0; i < len(command); {
		ch := command[i]
		if ch == '\\' && quote != '\'' {
			if i+1 < len(command) && strings.IndexByte(shellControlChars, command[i+1]) >= 0 {
				reinterpretable = true
			}
			i += 2
			continue
		}
		switch {
		case quote != 0:
			switch {
			case ch == quote:
				quote = 0
			case quote == '"' && (ch == '`' || ch == '$'):
				return true
			case strings.IndexByte(shellControlChars, ch) >= 0:
				reinterpretable = true
			}
		case ch == '\'' || ch == '"':
			quote = ch
		case ch == '$':
			if i+1 < len(command) && command[i+1] == '(' {
				return true
			}
		case strings.IndexByte(shellControlChars, ch) >= 0 && ch != '(' && ch != ')':
			return true
		}
		i++
	}
	if quote != 0 {
		return true
	}
	return reinterpretable && reinterpretedArgumentRe.MatchString(command)
}

func AllowlistMatch(command string, permanent []string) bool {
	command = pyStrip(command)
	if command == "" || AllowlistOperator(command) {
		return false
	}
	for _, pattern := range permanent {
		pattern = pyStrip(pattern)
		if pattern != "" && (command == pattern || (hasGlobMeta(pattern) && globMatch(command, pattern))) {
			return true
		}
	}
	return false
}
