package git

import (
	"errors"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/files"
)

// Ambient git environment the review pane keeps forwarding, the SSH and askpass helpers and the prompt switch
var simpleGitAllowedEnv = setOf("git_ssh_command", "git_ssh", "git_askpass", "ssh_askpass", "git_terminal_prompt")

// Environment keys simple-git guards besides every GIT_ key
var gitEnvKeys = setOf("editor", "git_askpass", "git_config_global", "git_config_system", "git_config_count", "git_config_parameters", "git_config", "git_editor", "git_exec_path", "git_external_diff", "git_pager", "git_proxy_command", "git_template_dir", "git_sequence_editor", "git_ssh", "git_ssh_command", "pager", "prefix", "ssh_askpass", "visual")

// Git command that exited non zero with stderr output, its message stdout then stderr
type GitError struct{ Message string }

func (e *GitError) Error() string { return e.Message }

// simpleGit runs git in a repository with the guards and result rules of the simple-git instance the review pane builds
type simpleGit struct {
	cwd string
	bin string
}

// Git runner for cwd, refused when cwd is not an existing directory
func gitFor(cwd, gitBin string) (*simpleGit, error) {
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return nil, errors.New("Cannot use simple-git on a directory that does not exist")
	}
	bin := gitBin
	if bin == "" {
		bin = "git"
	}
	return &simpleGit{cwd: cwd, bin: bin}, nil
}

// Process environment without guarded git keys outside the allow list, abbreviated options refused
func simpleGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		norm := jsTrim(jsLower(key))
		if (strings.HasPrefix(norm, "git_") || gitEnvKeys[norm]) && !simpleGitAllowedEnv[norm] {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_TEST_DISALLOW_ABBREVIATED_OPTIONS=true")
}

// Output of one git command, rejected when blocked as unsafe or when git exits non zero with stderr
func (g *simpleGit) run(args []string, trimmed bool) (string, error) {
	if v := blockedArgv(args); v != nil {
		return "", errors.New(v.message)
	}
	res, err := files.ExecGit(g.bin, args, g.cwd, simpleGitEnv(), 0)
	if err != nil {
		return "", err
	}
	if res.Code != 0 && res.Stderr != "" {
		return "", &GitError{Message: res.Stdout + res.Stderr}
	}
	if trimmed {
		return jsTrim(res.Stdout), nil
	}
	return res.Stdout, nil
}

func (g *simpleGit) raw(args ...string) (string, error) { return g.run(args, false) }

func (g *simpleGit) revparse(args ...string) (string, error) {
	return g.run(append([]string{"rev-parse"}, args...), true)
}

func (g *simpleGit) diff(args ...string) (string, error) {
	return g.run(append([]string{"diff"}, args...), false)
}

// Diff summary in the format the argv selects, the stat format when it selects none
func (g *simpleGit) diffSummary(args ...string) (DiffResult, error) {
	format := logFormatFromArgs(args)
	commands := []string{"diff"}
	if format == "" {
		format = "--stat"
		commands = append(commands, "--stat=4096")
	}
	commands = append(commands, args...)
	var flags []string
	for _, c := range commands {
		if logFormatRe.MatchString(c) {
			flags = append(flags, c)
		}
	}
	if len(flags) > 1 {
		return DiffResult{}, errors.New("Summary flags are mutually exclusive - pick one of " + strings.Join(flags, ","))
	}
	for _, c := range commands {
		if len(flags) == 1 && c == "-z" {
			return DiffResult{}, errors.New("Summary flag " + flags[0] + " parsing is not compatible with null termination option '-z'")
		}
	}
	out, err := g.run(commands, false)
	if err != nil {
		return DiffResult{}, err
	}
	return parseDiffSummary(format, out), nil
}

// Porcelain status with branch line, the argv extras after the defaults and NUL switches dropped
func (g *simpleGit) status(args ...string) (StatusResult, error) {
	commands := []string{"status", "--porcelain", "-b", "-u", "--null"}
	for _, a := range args {
		if a != "--null" && a != "-z" {
			commands = append(commands, a)
		}
	}
	out, err := g.run(commands, false)
	if err != nil {
		return StatusResult{}, err
	}
	return parseStatusSummary(out), nil
}

// Commits with each message part as its own -m, a message that is not text refused
func (g *simpleGit) commit(message any) error {
	var parts []string
	switch m := message.(type) {
	case string:
		parts = []string{m}
	case []any:
		for _, p := range m {
			s, ok := p.(string)
			if !ok {
				return errors.New("git.commit: requires the commit message to be supplied as a string/string[]")
			}
			parts = append(parts, s)
		}
	default:
		return errors.New("git.commit: requires the commit message to be supplied as a string/string[]")
	}
	args := []string{"-c", "core.abbrev=40", "commit"}
	for _, p := range parts {
		args = append(args, "-m", p)
	}
	_, err := g.run(args, false)
	return err
}

func (g *simpleGit) push() error {
	_, err := g.run([]string{"push", "--verbose", "--porcelain"}, false)
	return err
}
