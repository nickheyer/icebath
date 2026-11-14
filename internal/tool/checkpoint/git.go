package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/proc"
)

var supervisedIdentityVars = []string{
	"ICEBATH_LAUNCHD_LABEL", "ICEBATH_GATEWAY_EXTERNAL_SUPERVISOR", "NOTIFY_SOCKET", "WATCHDOG_USEC", "WATCHDOG_PID",
}

var kanbanEnvKeys = []string{
	"ICEBATH_KANBAN_TASK", "ICEBATH_KANBAN_RUN_ID", "ICEBATH_KANBAN_CLAIM_LOCK",
	"ICEBATH_KANBAN_GOAL_MODE", "ICEBATH_KANBAN_GOAL_MAX_TURNS",
}

const delegatedChildMarker = "ICEBATH_DELEGATED_CHILD_CONTEXT"

type GitError struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *GitError) Error() string {
	if e.Stderr != "" {
		return e.Stderr
	}
	return fmt.Sprintf("git %s exited with status %d", strings.Join(e.Args, " "), e.Code)
}

type RunError struct {
	msg string
	err error
}

func (e *RunError) Error() string { return e.msg }

func (e *RunError) Unwrap() error { return e.err }

func exitCode(err error) (int, bool) {
	var ge *GitError
	if errors.As(err, &ge) {
		return ge.Code, true
	}
	return 0, false
}

func exitedWith(err error, codes ...int) bool {
	code, ok := exitCode(err)
	return ok && slices.Contains(codes, code)
}

type gitOpts struct {
	index string
	scale int
	extra map[string]string
}

func (m *Manager) baseEnv() map[string]string {
	env := environMap(os.Environ())
	switch strings.ToLower(strings.TrimSpace(env[envKey("ICEBATH_GATEWAY_EXTERNAL_SUPERVISOR")])) {
	case "1", "true", "yes", "on":
		env[envKey("ICEBATH_GATEWAY_PROFILE_PIN")] = "1"
	default:
		if env[envKey("ICEBATH_SUPERVISED_CHILD")] != "" {
			env[envKey("ICEBATH_GATEWAY_PROFILE_PIN")] = "1"
		}
	}
	for _, name := range supervisedIdentityVars {
		delete(env, envKey(name))
	}
	if m.delegated || env[envKey(delegatedChildMarker)] != "" || env[envKey("ICEBATH_KANBAN_TASK")] != "" {
		inherited := env[envKey(delegatedChildMarker)]
		for _, name := range kanbanEnvKeys {
			delete(env, envKey(name))
		}
		if env[envKey("ICEBATH_SESSION_SOURCE")] == "kanban" {
			delete(env, envKey("ICEBATH_SESSION_SOURCE"))
		}
		if inherited == "" || inherited == "1" {
			inherited = fencedKanbanRoot()
		}
		env[envKey(delegatedChildMarker)] = inherited
	}
	return env
}

func fencedKanbanRoot() string {
	if override := strings.TrimSpace(os.Getenv("ICEBATH_KANBAN_HOME")); override != "" {
		return home.ExpandUser(override)
	}
	root, err := home.DefaultRoot()
	if err != nil {
		return "1"
	}
	return root
}

func environMap(list []string) map[string]string {
	env := make(map[string]string, len(list))
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		env[envKey(k)] = v
	}
	return env
}

func environList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

func isolateConfig(env map[string]string) {
	env[envKey("GIT_CONFIG_GLOBAL")] = os.DevNull
	env[envKey("GIT_CONFIG_SYSTEM")] = os.DevNull
	env[envKey("GIT_CONFIG_NOSYSTEM")] = "1"
}

func (m *Manager) runGit(ctx context.Context, store, workdir string, args []string, o gitOpts) (string, error) {
	workdir, err := canonical(workdir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(workdir)
	if err != nil {
		return "", &RunError{msg: "working directory not found: " + workdir, err: err}
	}
	if !info.IsDir() {
		return "", &RunError{msg: "working directory is not a directory: " + workdir}
	}
	env, err := m.selectGit(ctx, m.baseEnv())
	if err != nil {
		return "", err
	}
	env[envKey("GIT_DIR")] = store
	env[envKey("GIT_WORK_TREE")] = workdir
	delete(env, envKey("GIT_NAMESPACE"))
	delete(env, envKey("GIT_ALTERNATE_OBJECT_DIRECTORIES"))
	if o.index != "" {
		env[envKey("GIT_INDEX_FILE")] = o.index
	} else {
		delete(env, envKey("GIT_INDEX_FILE"))
	}
	isolateConfig(env)
	for k, v := range o.extra {
		env[envKey(k)] = v
	}
	return m.execGit(ctx, env, workdir, args, max(1, o.scale))
}

func (m *Manager) execGit(ctx context.Context, env map[string]string, dir string, args []string, scale int) (string, error) {
	git := lookPath(env[envKey("PATH")], "git")
	if git == "" {
		return "", &RunError{msg: "git is not installed or not on PATH"}
	}
	timeout := m.timeout * time.Duration(scale)
	cmd := exec.Command(git, args...)
	cmd.Env = environList(env)
	cmd.Dir = dir
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := proc.Run(rctx, cmd)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		if errors.Is(rctx.Err(), context.DeadlineExceeded) {
			return "", &RunError{msg: fmt.Sprintf("git timed out after %ds: %s", int(timeout/time.Second), strings.Join(cmd.Args, " ")), err: err}
		}
		var pe *fs.PathError
		if errors.As(err, &pe) && pe.Op == "chdir" {
			return "", &RunError{msg: "working directory not found: " + dir, err: err}
		}
		if errors.Is(err, fs.ErrNotExist) {
			return "", &RunError{msg: "git not found", err: err}
		}
		return "", &RunError{msg: err.Error(), err: err}
	}
	raw := slices.Contains(args, "-z")
	stdout := textOutput(res.Stdout)
	if raw {
		stdout = string(res.Stdout)
	}
	if !res.Exit.Success() {
		return stdout, &GitError{Args: args, Code: res.Exit.Code, Stderr: textOutput(res.Stderr)}
	}
	return stdout, nil
}

func textOutput(b []byte) string {
	s := strings.ToValidUTF8(string(b), "�")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.TrimSpace(s)
}

func (m *Manager) initBare(ctx context.Context, store string) error {
	env, err := m.selectGit(ctx, m.baseEnv())
	if err != nil {
		return err
	}
	isolateConfig(env)
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_NAMESPACE", "GIT_ALTERNATE_OBJECT_DIRECTORIES"} {
		delete(env, envKey(k))
	}
	if lookPath(env[envKey("PATH")], "git") == "" {
		return errors.New("Shadow store init failed: git is not installed or not on PATH")
	}
	if _, err := m.execGit(ctx, env, "", []string{"init", "--bare", store}, 1); err != nil {
		return fmt.Errorf("Shadow store init failed: %w", err)
	}
	return nil
}

func (m *Manager) gitAvailable(ctx context.Context) (bool, error) {
	env, err := m.selectGit(ctx, m.baseEnv())
	if err != nil {
		return false, err
	}
	return lookPath(env[envKey("PATH")], "git") != "", nil
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return true
}

func splitSearchPath(pathValue string) []string {
	var dirs []string
	for _, dir := range filepath.SplitList(pathValue) {
		if dir != "" {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}
