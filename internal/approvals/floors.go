package approvals

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	approvalsconfig "github.com/nickheyer/icebath/internal/approvals/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// What no mode, yolo or answer lets through: the hardline rules, deleting icebath's own program, guessing the sudo password on stdin and the
// user's always-deny patterns; and the allowlist matching that lets whole commands through

// Always-deny pattern the command matches under any of its spellings, empty when none
func (r *reader) userDeny(cfg *config.Config, command string) string {
	var patterns []string
	for _, p := range cfg.GetStringSlice(approvalsconfig.Deny) {
		if p = strings.TrimSpace(p); p != "" {
			patterns = append(patterns, p)
		}
	}
	if len(patterns) == 0 {
		return ""
	}
	for _, variant := range r.denyVariants(command) {
		candidate := strings.ToLower(strings.TrimSpace(variant))
		for _, pattern := range patterns {
			if tools.Glob(candidate, strings.ToLower(pattern)) {
				return pattern
			}
		}
	}
	return ""
}

func userDenyRefusal(pattern string) *tools.Refusal {
	return &tools.Refusal{Outcome: "user_deny", Message: "BLOCKED: this command matches the user-defined deny rule '" + pattern + "' (approvals.deny in " +
		"config.yaml). It cannot be executed via the agent — not even with --yolo, /yolo, or approvals.mode=off. Do NOT retry or rephrase this " +
		"command; the user has explicitly forbidden it."}
}

var blockedScript = regexp.MustCompile(`^blocked-.*\.sh$`)

// Saves a command too large to read as a script the user can review and run, empty when it can't be saved
func saveBlocked(cfg *config.Config, command string) string {
	dir := filepath.Join(cfg.ProfileDir(cfg.Profile()), "cache", "blocked-scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	if entries, err := os.ReadDir(dir); err == nil {
		cutoff := time.Now().Add(-7 * 24 * time.Hour)
		for _, entry := range entries {
			if info, err := entry.Info(); err == nil && !entry.IsDir() && blockedScript.MatchString(entry.Name()) && info.ModTime().Before(cutoff) {
				os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return ""
	}
	path := filepath.Join(dir, "blocked-"+strconv.FormatInt(time.Now().Unix(), 10)+"-"+hex.EncodeToString(suffix[:])+".sh")
	body := "#!/usr/bin/env bash\n# Auto-saved by icebath: this command exceeded the inline command\n# parser limit and was blocked from direct " +
		"execution. Review it,\n# then run it via: bash " + path + "\n" + command
	if !strings.HasSuffix(command, "\n") {
		body += "\n"
	}
	if os.WriteFile(path, []byte(strings.ToValidUTF8(body, "�")), 0o644) != nil {
		return ""
	}
	return path
}

const recovery = " RECOVERY: this block fires on oversized/unparseable inline command payloads (heredocs, giant one-liners), not on the operation itself. "

func hardlineRefusal(cfg *config.Config, description, command string) *tools.Refusal {
	message := "BLOCKED (hardline): " + description + ". This command is on the unconditional blocklist and cannot be executed via the agent — not " +
		"even with --yolo, /yolo, approvals.mode=off, or cron approve mode. If you genuinely need to run it, run it yourself in a terminal outside " +
		"the agent."
	if description == parserLimit || description == malformedExec {
		if saved := saveBlocked(cfg, command); command != "" && saved != "" {
			message += recovery + "Your command was saved to " + saved + " — review it, then run: terminal(command=\"bash " + saved + "\"). Do not retry inline."
		} else {
			message += recovery + "Write the script to a file with write_file, then run it: terminal(command=\"bash /path/script.sh\") or \"python3 " +
				"/path/script.py\". Do not retry inline."
		}
	}
	return &tools.Refusal{Outcome: "hardline", Message: message}
}

func sudoRefusal(description string) *tools.Refusal {
	return &tools.Refusal{Outcome: "hardline", Message: "BLOCKED: " + description + ". Do not pipe passwords to 'sudo -S' — this is a brute-force " +
		"attack vector. Set Sudo password in /sandbox (or SUDO_PASSWORD in the environment icebath runs in) if the agent " +
		"needs passwordless sudo, or run the sudo command manually in your own terminal."}
}

var (
	runtimeSegments = regexp.MustCompile(`\n|&&|\|\||[;|]`)
	runtimePrefixes = strs.Set("sudo", "command", "nohup", "exec", "env", "nice", "time")
	deleters        = strs.Set("rm", "rmdir", "rd", "del", "erase", "remove-item", "ri")
	findDeletes     = regexp.MustCompile(`(?i)(?:^|\s)-(?:delete|exec\s+rm\b)`)
	posixDrive      = regexp.MustCompile(`^/(?:(mnt)/)?([a-zA-Z])/([^\n]+)\n?$`)
)

// One form of a path for comparing with icebath's own program: quotes, ~ and variables expanded, links followed
func runtimePath(raw string) string {
	path := strings.TrimSpace(raw)
	if path == "" {
		return ""
	}
	if len(path) >= 2 && path[0] == path[len(path)-1] && (path[0] == '"' || path[0] == '\'') {
		path = path[1 : len(path)-1]
	}
	path = os.ExpandEnv(fsx.ExpandHome(path))
	if runtime.GOOS == "windows" {
		if m := posixDrive.FindStringSubmatch(path); m != nil {
			path = m[2] + `:\` + m[3]
		}
		path = strings.ReplaceAll(path, "/", `\`)
	}
	path = resolve(filepath.Clean(path))
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

// Program icebath runs from, as runtimePath spells it
var program = func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return runtimePath(exe)
}()

func overlap(a, b string) bool {
	sep := string(filepath.Separator)
	return a != "" && b != "" && (a == b || strings.HasPrefix(a, b+sep) || strings.HasPrefix(b, a+sep))
}

// What of icebath's own program the command would delete, empty when nothing
func deletesRuntime(command string) string {
	if strings.TrimSpace(command) == "" || program == "" {
		return ""
	}
	hit := func(word string) bool { return overlap(runtimePath(word), program) }
	for _, segment := range runtimeSegments.Split(command, -1) {
		words, err := splitPunct(segment, "", false)
		if err != nil {
			words = strings.Fields(segment)
		}
		for len(words) > 0 && (runtimePrefixes[words[0]] || strings.Contains(words[0], "=") && !strings.HasPrefix(words[0], "-") &&
			!strings.HasPrefix(words[0], "/") && envAssign.MatchString(words[0])) {
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		name := strings.ToLower(base(strings.ReplaceAll(words[0], `\`, "/")))
		if name == "find" && findDeletes.MatchString(segment) || deleters[name] {
			for _, word := range words[1:] {
				if !strings.HasPrefix(word, "-") && hit(word) {
					return "the icebath binary this runtime is running from"
				}
			}
		}
	}
	return ""
}

// What no mode, yolo or answer lets through, nil when none of it applies
func (r *reader) floor(cfg *config.Config, command string, sudoPassword bool) *tools.Refusal {
	if description := r.hardline(command); description != "" {
		slog.Warn("Hardline block", "description", description, "command", preview(command))
		return hardlineRefusal(cfg, description, command)
	}
	if target := deletesRuntime(command); target != "" {
		slog.Warn("Runtime self-delete block", "target", target, "command", preview(command))
		return hardlineRefusal(cfg, "recursive/any delete of "+target, command)
	}
	if description := r.sudoGuess(command, sudoPassword); description != "" {
		slog.Warn("Sudo stdin guard block", "description", description, "command", preview(command))
		return sudoRefusal(description)
	}
	if pattern := r.userDeny(cfg, command); pattern != "" {
		slog.Warn("User deny rule blocked command", "rule", pattern, "command", preview(command))
		return userDenyRefusal(pattern)
	}
	return nil
}

// Command cut short for the log
func preview(command string) string {
	if runes := []rune(command); len(runes) > 200 {
		return string(runes[:200])
	}
	return command
}

const shellControls = "\n\r;&|<>`$()"

var reinterpreted = regexp.MustCompile(`(?:^|[ \t])(?:-[^-\s]*[ce]|--(?:command|eval))(?:[= \t]|$)`)

// Whether the command is too compound for an allowlisted command or glob to let it through, quotes taken into account
func compound(command string) bool {
	var quote rune
	escapedControl := false
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		if ch == '\\' && quote != '\'' {
			if i+1 < len(runes) && strings.ContainsRune(shellControls, runes[i+1]) {
				escapedControl = true
			}
			i++
			continue
		}
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			} else if quote == '"' && (ch == '`' || ch == '$') {
				return true
			} else if strings.ContainsRune(shellControls, ch) {
				escapedControl = true
			}
		case ch == '\'' || ch == '"':
			quote = ch
		case ch == '$':
			if i+1 < len(runes) && runes[i+1] == '(' {
				return true
			}
		case strings.ContainsRune(shellControls, ch) && ch != '(' && ch != ')':
			return true
		}
	}
	return quote != 0 || escapedControl && reinterpreted.MatchString(command)
}

// Whether the allowlist holds the command as it is, or a glob it matches
func allowlisted(cfg *config.Config, command string) bool {
	command = strings.TrimSpace(command)
	if command == "" || compound(command) {
		return false
	}
	for _, pattern := range cfg.GetStringSlice(approvalsconfig.Allowlist) {
		if pattern = strings.TrimSpace(pattern); pattern != "" && (command == pattern || strings.ContainsAny(pattern, "*?[") && tools.Glob(command, pattern)) {
			return true
		}
	}
	return false
}
