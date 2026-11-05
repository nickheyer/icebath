package tools

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
)

// Matching approvals.deny glob, empty when none, run over the deobfuscated variants
func matchUserDenyRule(env *Env, command string) string {
	raw := getApprovalConfig(env)["deny"]
	var globs []string
	if list, ok := raw.([]any); ok {
		for _, p := range list {
			if s, ok := p.(string); ok && strings.TrimSpace(s) != "" {
				globs = append(globs, strings.TrimSpace(s))
			}
		}
	} else if list, ok := raw.([]string); ok {
		for _, s := range list {
			if strings.TrimSpace(s) != "" {
				globs = append(globs, strings.TrimSpace(s))
			}
		}
	}
	if len(globs) == 0 {
		return ""
	}
	for _, variant := range DenyCommandVariants(command) {
		candidate := strings.TrimSpace(strings.ToLower(variant))
		for _, pattern := range globs {
			if fnmatch.FnmatchCase(candidate, strings.ToLower(pattern)) {
				return pattern
			}
		}
	}
	return ""
}

// Standard block result for an approvals.deny match
func userDenyBlockResult(pattern string) GuardResult {
	return GuardResult{
		Approved:    false,
		PatternKey:  "user_deny:" + pattern,
		Description: pattern,
		Message: "BLOCKED: this command matches the user-defined deny rule '" + pattern + "' (approvals.deny in config.yaml). It cannot be " +
			"executed via the agent — not even with --yolo, /yolo, or " +
			"approvals.mode=off. Do NOT retry or rephrase this command; the user has explicitly forbidden it.",
	}
}

var blockedScriptGlob = regexp.MustCompile(`^blocked-.*\.sh$`)

// Removes files in a directory older than the age whose names match the glob
func unlinkFilesOlderThan(dir string, match *regexp.Regexp, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() || !match.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		os.Remove(filepath.Join(dir, entry.Name()))
	}
}

// Persists a parser limit blocked command as a runnable script, empty on any failure
func saveBlockedPayload(env *Env, command string) string {
	scriptDir := filepath.Join(envHome(env), "cache", "blocked-scripts")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		return ""
	}
	unlinkFilesOlderThan(scriptDir, blockedScriptGlob, 7*24*time.Hour)
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return ""
	}
	path := filepath.Join(scriptDir, "blocked-"+strconv.FormatInt(time.Now().Unix(), 10)+"-"+hex.EncodeToString(suffix[:])+".sh")
	body := "#!/usr/bin/env bash\n" +
		"# Auto-saved by Icebath: this command exceeded the inline command\n" +
		"# parser limit and was blocked from direct execution. Review it,\n" +
		"# then run it via: bash " + path + "\n" + command
	if !strings.HasSuffix(command, "\n") {
		body += "\n"
	}
	if err := os.WriteFile(path, []byte(strings.ToValidUTF8(body, "�")), 0o644); err != nil {
		return ""
	}
	return path
}

const recoveryPrefix = " RECOVERY: this block fires on oversized/unparseable inline " +
	"command payloads (heredocs, giant one-liners), not on the operation itself. "

// Standard block result for a hardline match with recovery guidance for parser limit blocks
func hardlineBlockResult(env *Env, description, command string) GuardResult {
	message := "BLOCKED (hardline): " + description + ". " +
		"This command is on the unconditional blocklist and cannot " +
		"be executed via the agent — not even with --yolo, /yolo, " +
		"approvals.mode=off, or cron approve mode. If you genuinely " +
		"need to run it, run it yourself in a terminal outside the agent."
	if description == ParserLimitDescription || description == MalformedExecDescription {
		saved := ""
		if command != "" {
			saved = saveBlockedPayload(env, command)
		}
		if saved != "" {
			message += recoveryPrefix + "Your command was saved to " + saved + " — review it, then run: terminal(command=\"bash " + saved + "\"). " +
				"Do not retry inline."
		} else {
			message += recoveryPrefix + "Write the script to a file with write_file, " +
				"then run it: terminal(command=\"bash /path/script.sh\") or " +
				"\"python3 /path/script.py\". Do not retry inline."
		}
	}
	return GuardResult{Approved: false, PatternKey: "hardline", Description: description, Message: message}
}

// Standard block result for the sudo stdin guard
func sudoStdinBlockResult(description string) GuardResult {
	return GuardResult{Approved: false, PatternKey: "hardline", Description: description, Message: "BLOCKED: " + description + ". " +
		"Do not pipe passwords to 'sudo -S' — this is a brute-force " +
		"attack vector. Set SUDO_PASSWORD in your .env file if the " +
		"agent needs passwordless sudo, or run the sudo command manually in your own terminal."}
}

const allowlistShellControlChars = "\n\r;&|<>`$()"

var reinterpretedArgumentRe = regexp.MustCompile(`(?:^|[ \t])(?:-[^-\s]*[ce]|--(?:command|eval))(?:[= \t]|$)`)

// Reports whether a command is too compound for the allowlist shortcut, quote aware
func hasAllowlistShellOperator(command string) bool {
	var quote rune
	hasReinterpretable := false
	runes := []rune(command)
	n := len(runes)
	i := 0
	for i < n {
		ch := runes[i]
		if ch == '\\' && quote != '\'' {
			if i+1 < n && strings.ContainsRune(allowlistShellControlChars, runes[i+1]) {
				hasReinterpretable = true
			}
			i += 2
			continue
		}
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			} else if quote == '"' && (ch == '`' || ch == '$') {
				return true
			} else if strings.ContainsRune(allowlistShellControlChars, ch) {
				hasReinterpretable = true
			}
		case ch == '\'' || ch == '"':
			quote = ch
		case ch == '$':
			if i+1 < n && runes[i+1] == '(' {
				return true
			}
		case strings.ContainsRune(allowlistShellControlChars, ch) && ch != '(' && ch != ')':
			return true
		}
		i++
	}
	if quote != 0 {
		return true
	}
	return hasReinterpretable && reinterpretedArgumentRe.MatchString(command)
}

// Reports whether the permanent allowlist holds the exact command text or a matching glob
func commandMatchesPermanentAllowlist(env *Env, command string) bool {
	command = strings.TrimSpace(command)
	if command == "" || hasAllowlistShellOperator(command) {
		return false
	}
	approvalState.Lock()
	patterns := make([]string, 0)
	for p := range permanentSet(env) {
		patterns = append(patterns, p)
	}
	approvalState.Unlock()
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if command == pattern || (strings.ContainsAny(pattern, "*?[") && fnmatch.FnmatchCase(command, pattern)) {
			return true
		}
	}
	return false
}
