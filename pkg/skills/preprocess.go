package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

var skillTemplateRe = regexp.MustCompile(`\$\{((?:ICEBATH|HERMES)_(?:SKILL_DIR|SESSION_ID))\}`)

var inlineShellRe = regexp.MustCompile("!`([^`\n]+)`")

const inlineShellMaxOutput = 4000

// Replaces skill dir and session id template tokens, tokens without a value stay in place
func SubstituteTemplateVars(content, skillDir, sessionID string) string {
	if content == "" {
		return content
	}
	return skillTemplateRe.ReplaceAllStringFunc(content, func(m string) string {
		token := skillTemplateRe.FindStringSubmatch(m)[1]
		var v string
		if strings.HasSuffix(token, "_SKILL_DIR") {
			v = skillDir
		} else {
			v = sessionID
		}
		if v == "" {
			return m
		}
		return v
	})
}

// Git Bash resolver on Windows, a seam the inline shell tests replace
var findBash = tools.FindBash

// Runs one inline shell snippet returning trimmed stdout, stderr when stdout is empty, or a failure marker
func (s *Scope) RunInlineShell(ctx context.Context, command, cwd string, timeout int) string {
	bash := "bash"
	if runtime.GOOS == "windows" {
		found, err := findBash()
		if err != nil {
			return fmt.Sprintf("[inline-shell error: %v]", err)
		}
		bash = found
	}
	if cwd != "" {
		if marker := inlineShellCwdError(cwd); marker != "" {
			return marker
		}
	}
	if _, err := exec.LookPath(bash); err != nil {
		return "[inline-shell error: bash not found]"
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(max(1, timeout))*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bash, "-c", command)
	cmd.Dir = cwd
	cmd.Stdin = nil
	cmd.WaitDelay = time.Second
	cmd.Env = envSlice(tools.DelegatedChildSubprocessEnv(s.Env, environMap()))
	hideInlineShellWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("[inline-shell timeout after %ds: %s]", timeout, command)
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		var errno syscall.Errno
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "[inline-shell error: bash not found]"
		case errors.As(err, &errno):
			return fmt.Sprintf("[inline-shell error: %s]", spawnErrorText(errno, bash))
		}
		return fmt.Sprintf("[inline-shell error: %v]", err)
	}
	output := strings.TrimRight(universalNewlines(pystr.DecodeUTF8Replace(stdout.Bytes())), "\n")
	if output == "" {
		output = strings.TrimRight(universalNewlines(pystr.DecodeUTF8Replace(stderr.Bytes())), "\n")
	}
	if code := pyReturnCode(cmd.ProcessState); code != 0 && output == "" {
		return fmt.Sprintf("[inline-shell exit %d with no output: %s]", code, command)
	}
	return tools.Elide(output, inlineShellMaxOutput)
}

// Text mode newline translation of a captured stream, CRLF and lone CR becoming LF
func universalNewlines(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// Python OSError text of a spawn errno, Windows CreateProcess failures carrying no filename
func spawnErrorText(errno syscall.Errno, filename string) string {
	text, _ := pypath.ErrnoText(errno)
	if runtime.GOOS == "windows" {
		return text
	}
	return text + ": " + pyrepr.StrRepr(filename)
}

// Process environment as a map
func environMap() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// Environment map as KEY=VALUE pairs
func envSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// Replaces every inline shell snippet with its output, run with the skill dir as working dir
func (s *Scope) ExpandInlineShell(ctx context.Context, content, skillDir string, timeout int) string {
	if !strings.Contains(content, "!`") {
		return content
	}
	return inlineShellRe.ReplaceAllStringFunc(content, func(m string) string {
		cmd := strings.TrimSpace(inlineShellRe.FindStringSubmatch(m)[1])
		if cmd == "" {
			return ""
		}
		return s.RunInlineShell(ctx, cmd, skillDir, timeout)
	})
}

// Reports whether a skill dir is a community trust hub install, true when its owning lock is unreadable
func (s *Scope) isCommunityHubSkill(skillDir string) bool {
	if skillDir == "" {
		return false
	}
	target := resolvePath(skillDir)
	type lockRoot struct{ lock, root string }
	locks := []lockRoot{{resolvePath(filepath.Join(s.SkillsDir(), ".hub", "lock.json")), resolvePath(s.SkillsDir())}}
	seen := map[string]bool{locks[0].lock: true}
	for root := filepath.Dir(target); ; root = filepath.Dir(root) {
		lock := filepath.Join(root, ".hub", "lock.json")
		if filepath.Base(root) == "skills" && isFile(lock) {
			if key := resolvePath(lock); !seen[key] {
				seen[key] = true
				locks = append(locks, lockRoot{key, root})
			}
		}
		if filepath.Dir(root) == root {
			break
		}
	}
	for _, lr := range locks {
		if !isRelativeTo(target, lr.root) || !isFile(lr.lock) {
			continue
		}
		entries, err := hubLockEntries(lr.lock)
		if err != nil {
			s.logger().Debug(fmt.Sprintf("Unreadable hub lock %s; inline shell stays off for its tree", lr.lock), "error", err)
			return true
		}
		for _, e := range entries {
			rel := pyStrOr(firstTruthy(e["install_path"]))
			if e["trust_level"] == "community" && rel != "" && isRelativeTo(target, resolvePath(filepath.Join(lr.root, rel))) {
				return true
			}
		}
	}
	return false
}

// Mapping entries of a hub lock file's installed table
func hubLockEntries(lockPath string) ([]map[string]any, error) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF")), &doc); err != nil {
		return nil, err
	}
	raw, ok := doc["installed"]
	if !ok {
		return nil, errors.New("KeyError: 'installed'")
	}
	installed, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("installed is %T, not a mapping", raw)
	}
	var out []map[string]any
	for _, v := range installed {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// Python int of a config value, an error where int() raises
func pyIntValue(v any) (int, error) {
	switch t := v.(type) {
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return 0, fmt.Errorf("cannot convert float %v to integer", t)
		}
		return int(t), nil
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(strings.ReplaceAll(t, "_", "")))
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: '%s'", t)
		}
		return n, nil
	}
	return 0, fmt.Errorf("int() argument must be a string or a real number, not '%T'", v)
}

// Applies configured template and inline shell preprocessing, the raw content when preprocessing fails
func (s *Scope) PreprocessSkillContent(ctx context.Context, content, skillDir, sessionID string, skillsCfg map[string]any) string {
	out, err := s.preprocessSkillContent(ctx, content, skillDir, sessionID, skillsCfg)
	if err != nil {
		s.logger().Debug("Could not preprocess skill content", "skill_dir", skillDir, "error", err)
		return content
	}
	return out
}

// Template and inline shell preprocessing, an error where the Python raises
func (s *Scope) preprocessSkillContent(ctx context.Context, content, skillDir, sessionID string, cfg map[string]any) (string, error) {
	if content == "" {
		return content, nil
	}
	if cfg == nil {
		cfg = s.skillsCfg()
		if cfg == nil {
			cfg = map[string]any{}
		}
	}
	if v, ok := cfg["template_vars"]; !ok || fmTruthy(v) {
		content = SubstituteTemplateVars(content, skillDir, sessionID)
	}
	if fmTruthy(cfg["inline_shell"]) && !s.isCommunityHubSkill(skillDir) {
		raw, ok := cfg["inline_shell_timeout"]
		if !ok || !fmTruthy(raw) {
			raw = 10
		}
		timeout, err := pyIntValue(raw)
		if err != nil {
			return "", err
		}
		content = s.ExpandInlineShell(ctx, content, skillDir, timeout)
	}
	return content, nil
}
