package terminal

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	sshconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminals/ssh/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
)

type registrant struct {
	skills *skills.Registry
}

func Register(registry *terminal.Registry, skillset *skills.Registry) {
	registry.Register(registrant{skills: skillset})
}

func (r registrant) Backends() []terminal.Backend {
	return []terminal.Backend{{
		Name:    "ssh",
		Title:   "SSH",
		About:   "Run commands on a remote machine over SSH",
		Where:   "a remote host reached over SSH (likely Linux)",
		Remote:  true,
		Home:    "~/.icebath",
		Cwd:     "~",
		Remap:   remap,
		Check:   check,
		Setup:   setup,
		Doctor:  doctor,
		Prepare: prepare,
		Streams: true,
		Create: func(ctx context.Context, spec *terminal.Spec) (terminal.Environment, error) {
			return create(ctx, spec, r.skills)
		},
	}}
}

// Machine and account commands run as, from the settings
type target struct {
	host, user string
	port       int
	// Private key file, empty for the SSH agent and default keys
	key string
}

func targetOf(cfg *config.Config) (target, error) {
	port, err := cast.ToIntE(cfg.Get(sshconfig.Port))
	if err != nil {
		return target{}, fmt.Errorf("%s holds %q, which is not a whole number; fix it in /settings Terminal", sshconfig.Port, cfg.GetString(sshconfig.Port))
	}
	if port < 1 || port > 65535 {
		return target{}, fmt.Errorf("%s is %d, which is not a port from 1 to 65535; fix it in /settings Terminal", sshconfig.Port, port)
	}
	t := target{host: strings.TrimSpace(cfg.GetString(sshconfig.Host)), user: strings.TrimSpace(cfg.GetString(sshconfig.User)), port: port}
	if key := strings.TrimSpace(cfg.GetString(sshconfig.KeyFile)); key != "" {
		t.key = terminal.ExpandPath(key)
	}
	return t, nil
}

func (t target) login() string { return t.user + "@" + t.host }

// Why the ssh client can't run here, nil when it is on PATH
func client() error {
	if _, err := exec.LookPath("ssh"); err != nil {
		return &terminal.MissingError{Reason: "SSH is not installed or not in PATH. Install OpenSSH client: apt install openssh-client"}
	}
	return nil
}

func check(cfg *config.Config) error {
	t, err := targetOf(cfg)
	if err != nil {
		return err
	}
	if t.host == "" || t.user == "" {
		return errors.New("the SSH host and user are not set; open /backends and enter them in the SSH settings, or run commands on this machine instead")
	}
	return client()
}

func setup(*config.Config) []string {
	return []string{
		"Run commands on a remote machine via SSH.",
		"Set the SSH host and user in the SSH settings, plus the port and key file when they differ from the defaults.",
		"Readying the backend tests the connection with your key, as commands will make it.",
		"Secrets reach the remote shell only for names the remote SSH server accepts (AcceptEnv).",
	}
}

func doctor(cfg *config.Config) []terminal.Check {
	t, err := targetOf(cfg)
	if err != nil {
		return []terminal.Check{{OK: false, Label: "SSH port invalid", Detail: "(" + err.Error() + ")"}}
	}
	if t.host == "" {
		return []terminal.Check{{OK: false, Label: "SSH host not configured", Detail: "(needed for the 'ssh' terminal backend)"}}
	}
	if err := client(); err != nil {
		return []terminal.Check{{OK: false, Label: "SSH client not installed", Detail: "(" + err.Error() + ")"}}
	}
	ok, detail := probe(context.Background(), t)
	return []terminal.Check{{OK: ok, Label: "SSH connection to " + t.host, Detail: detail}}
}

// Tests the connection the way commands make it
func prepare(ctx context.Context, cfg *config.Config, say func(string)) error {
	if err := check(cfg); err != nil {
		return err
	}
	t, _ := targetOf(cfg)
	say("Testing connection...")
	if ok, detail := probe(ctx, t); !ok {
		return fmt.Errorf("SSH connection failed: %s. Check your SSH key and host settings.", detail)
	}
	say("SSH connection successful!")
	return nil
}

// Runs echo ok over a fresh batch mode connection that takes a new host's key as commands do, answering whether it worked and why not
func probe(ctx context.Context, t target) (bool, string) {
	argv := []string{"ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=5"}
	if t.key != "" {
		argv = append(argv, "-i", t.key)
	}
	if t.port != 22 {
		argv = append(argv, "-p", strconv.Itoa(t.port))
	}
	login := t.host
	if t.user != "" {
		login = t.login()
	}
	result, err := terminal.Capture(ctx, append(argv, login, "echo ok"), 15*time.Second, nil)
	if err != nil {
		return false, err.Error()
	}
	if result.Code != 0 {
		return false, strings.TrimSpace(result.Stderr)
	}
	return true, ""
}

// Remote folder for a host folder: one under the home commands get here becomes the same place under the remote home
func remap(cfg *config.Config, cwd string) string {
	text := strings.TrimSpace(cwd)
	if text == "" {
		return "~"
	}
	if strings.HasPrefix(text, "~") {
		return text
	}
	env := terminal.ProcessEnv()
	home := terminal.SubprocessHome(cfg, env)
	if home == "" || !path.IsAbs(text) || path.Clean(home) == path.Clean(terminal.RealHome(cfg, env)) {
		return text
	}
	rel, err := filepath.Rel(path.Clean(home), path.Clean(text))
	if err != nil {
		return text
	}
	switch rel = filepath.ToSlash(rel); {
	case rel == ".":
		return "~"
	case rel == ".." || strings.HasPrefix(rel, "../"):
		return text
	}
	return "~/" + rel
}
