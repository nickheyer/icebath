package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	terminalconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal/config"
	"github.com/nickheyer/icebath/internal/tools"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

const toolset = "terminal"

type danger struct {
	pattern *regexp.Regexp
	reason  string
}

const start = `(?:^|[\n;&|(])\s*(?:sudo\s+)?`

var blocked = []danger{
	{regexp.MustCompile(start + `rm\s+(?:-\S+\s+)*["']?(?:/|/\*|~|~/|\$HOME|\$\{HOME\}|/(?:home|root|etc|usr|var|bin|sbin|boot|lib)(?:/\*)?/?)["']?(?:\s|$|[;&|)])`), "deletes a system or home directory"},
	{regexp.MustCompile(start + `mkfs(?:\.[a-z0-9]+)?\b`), "formats a filesystem"},
	{regexp.MustCompile(start + `dd\b[^\n]*\bof=/dev/(?:sd|nvme|hd|mmcblk|vd|xvd)`), "writes to a raw disk"},
	{regexp.MustCompile(`>\s*/dev/(?:sd|nvme|hd|mmcblk|vd|xvd)`), "writes to a raw disk"},
	{regexp.MustCompile(`:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`), "fork bomb"},
	{regexp.MustCompile(start + `kill\s+(?:-\S+\s+)*-1\b`), "kills every process"},
	{regexp.MustCompile(start + `(?:shutdown|reboot|halt|poweroff)\b`), "shuts down or restarts the machine"},
	{regexp.MustCompile(start + `(?:systemctl\s+(?:poweroff|reboot|halt|kexec)|init\s+[06])\b`), "shuts down or restarts the machine"},
}

var dangerous = []danger{
	{regexp.MustCompile(`\brm\s+(?:[^\n;|&]*\s)?-\S*[rR]`), "recursive delete"},
	{regexp.MustCompile(`\bchmod\s+(?:-\S+\s+)*(?:777|666|[oa]\+[rwx]*w)\b`), "world-writable permissions"},
	{regexp.MustCompile(`\bchown\s+(?:-\S*R\S*|--recursive)\s+\S*root`), "recursive chown to root"},
	{regexp.MustCompile(`\bdd\s+[^\n]*\bif=`), "disk copy"},
	{regexp.MustCompile(`(?i)\bDROP\s+(?:TABLE|DATABASE)\b`), "SQL DROP"},
	{regexp.MustCompile(`(?i)\bTRUNCATE\s+(?:TABLE\s+)?\w`), "SQL TRUNCATE"},
	{regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+[\w."]+\s*(?:;|["']|$)`), "SQL DELETE without WHERE"},
	{regexp.MustCompile(`>\s*/etc/`), "overwrites system config"},
	{regexp.MustCompile(`\bsystemctl\s+(?:-\S+\s+)*(?:stop|restart|disable|mask)\b`), "stops or restarts a system service"},
	{regexp.MustCompile(`\bpkill\s+-9\b|\bkillall\s+(?:-\S+\s+)*-(?:9|KILL|SIGKILL)\b`), "force kills processes"},
	{regexp.MustCompile(`\b(?:curl|wget)\b[^\n]*\|\s*(?:sudo\s+)?(?:ba|z|k|da)?sh\b`), "pipes remote content to a shell"},
	{regexp.MustCompile(`\b(?:ba|z|k|da)?sh\s+<\s*<?\s*\(\s*(?:curl|wget)\b`), "runs a remote script"},
	{regexp.MustCompile(`\btee\b[^\n]*(?:/etc/|\.ssh/|\.(?:bashrc|zshrc|profile|bash_profile|zprofile|netrc)\b)`), "overwrites a system or credential file"},
	{regexp.MustCompile(`>>?\s*["']?(?:~|\$HOME|\$\{HOME\})/\.(?:ssh/|bashrc|zshrc|profile|bash_profile|zprofile|netrc)`), "overwrites a credential or shell startup file"},
	{regexp.MustCompile(`\bxargs\s+[^\n]*\brm\b`), "xargs with rm"},
	{regexp.MustCompile(`\bfind\b[^\n]*(?:-delete\b|-exec(?:dir)?\s+(?:/\S*/)?rm\b)`), "find that deletes"},
	{regexp.MustCompile(`\bgit\s+reset\s+--hard\b`), "git reset --hard discards uncommitted changes"},
	{regexp.MustCompile(`\bgit\s+push\b[^\n]*(?:--force|\s-f\b)`), "git force push rewrites remote history"},
	{regexp.MustCompile(`\bgit\s+clean\s+-\S*f`), "git clean deletes untracked files"},
	{regexp.MustCompile(`\bgit\s+branch\s+-D\b`), "git branch force delete"},
	{regexp.MustCompile(`\bsudo\b`), "runs as root"},
	{regexp.MustCompile(`169\.254\.169\.254|metadata\.google\.internal`), "reads the cloud metadata endpoint"},
	{regexp.MustCompile(start + `(?:npm|pnpm|yarn|pip3?|brew)\s+(?:-\S+\s+)*(?:uninstall|remove|rm|un)\b`), "package uninstall"},
}

func match(list []danger, command string) string {
	for _, entry := range list {
		if entry.pattern.MatchString(command) {
			return entry.reason
		}
	}
	return ""
}

type registrant struct{}

func Register(registry *tools.Registry) {
	registry.Register(registrant{})
}

func (registrant) Tools() []tools.Def {
	return []tools.Def{{
		Name:    "terminal",
		Toolset: toolset,
		Schema: tools.Schema(
			"Run a shell command in the working directory and return its output and exit code. Every call starts a new shell, so a changed directory or variable does not carry over. The command is stopped when its timeout passes, so do not start servers or other background processes. For files, prefer read_file, search_files, patch and write_file over cat, grep, sed and echo.",
			[]string{"command"},
			map[string]any{
				"command": tools.Property("string", "The shell command to run"),
				"timeout": tools.Property("integer", "Seconds the command may run before it is stopped"),
			},
		),
		Handler: tools.Bind(terminal),
	}}
}

func terminal(ctx context.Context, call *tools.Call, args struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}) (string, error) {
	if reason := match(blocked, args.Command); reason != "" {
		return "", fmt.Errorf("blocked: %s", reason)
	}
	if reason := match(dangerous, args.Command); reason != "" {
		if err := call.Env.Approve(ctx, args.Command, reason); err != nil {
			return "", err
		}
	}
	cfg := call.Env.Config
	timeout := time.Duration(cfg.GetInt(terminalconfig.Timeout)) * time.Second
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	shell, err := exec.LookPath("bash")
	if err != nil {
		shell = "sh"
	}
	output := tools.NewCapture(cfg.GetInt(toolsconfig.OutputMaxBytes))
	command := exec.CommandContext(ctx, shell, "-c", args.Command)
	command.Dir = call.Env.Cwd
	command.Stdout, command.Stderr = output, output
	passthrough := cfg.GetStringSlice(terminalconfig.EnvPassthrough)
	command.Env = []string{}
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if slices.Contains(passthrough, name) || !strings.HasSuffix(name, "_API_KEY") && !strings.HasSuffix(name, "_AUTH_TOKEN") {
			command.Env = append(command.Env, variable)
		}
	}
	command.WaitDelay = time.Second
	group(command)
	err = command.Run()
	var exit *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		fmt.Fprintf(output, "\n[timed out after %s]", timeout)
	case ctx.Err() != nil:
		fmt.Fprint(output, "\n[interrupted]")
	case errors.As(err, &exit):
		fmt.Fprintf(output, "\n[exit code %d]", exit.ExitCode())
	case err != nil:
		return "", err
	}
	return output.String(), nil
}
