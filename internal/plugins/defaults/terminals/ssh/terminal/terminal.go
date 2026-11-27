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
	"github.com/nickheyer/icebath/pkg/strs"
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
		About:   "Another computer that commands reach over SSH",
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
		return target{}, fmt.Errorf("Port in /settings under Terminal › SSH is set to %q, which is not a whole number. Change it there.", cfg.GetString(sshconfig.Port))
	}
	if port < 1 || port > 65535 {
		return target{}, fmt.Errorf("Port in /settings under Terminal › SSH is %s, which is not a port from 1 to 65,535. Change it there.", strs.Number(float64(port)))
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
		return &terminal.MissingError{Reason: "The ssh program is not installed or not on your PATH. Install the OpenSSH client, such as with `apt install openssh-client`."}
	}
	return nil
}

func check(cfg *config.Config) error {
	t, err := targetOf(cfg)
	if err != nil {
		return err
	}
	if t.host == "" || t.user == "" {
		return errors.New("The SSH host and user are not set. Enter them in /settings under Terminal › SSH, or choose another place to run commands in /backends.")
	}
	return client()
}

func setup(*config.Config) []string {
	return []string{
		"Runs the agent's commands on another computer over SSH.",
		"Set Host and User in /settings under Terminal › SSH, and Port and Key file when they differ from the defaults.",
		"Preparing SSH on this screen tests the connection with your key, the way commands make it.",
		"Secret variables reach the other computer only when its SSH server accepts their names with AcceptEnv.",
	}
}

func doctor(cfg *config.Config) []terminal.Check {
	t, err := targetOf(cfg)
	if err != nil {
		return []terminal.Check{{OK: false, Label: "The SSH port is not valid.", Detail: err.Error()}}
	}
	if t.host == "" {
		return []terminal.Check{{OK: false, Label: "The SSH host is not set.", Detail: "Commands run over SSH only once it is."}}
	}
	if err := client(); err != nil {
		return []terminal.Check{{OK: false, Label: "The SSH client is not installed.", Detail: "Install OpenSSH, such as with `apt install openssh-client`."}}
	}
	if ok, detail := probe(context.Background(), t); !ok {
		return []terminal.Check{{OK: false, Label: "Could not connect to " + t.host + " over SSH.", Detail: "It said: " + reason(detail) + "."}}
	}
	return []terminal.Check{{OK: true, Label: "Connected to " + t.host + " over SSH."}}
}

// Tests the connection the way commands make it
func prepare(ctx context.Context, cfg *config.Config, say func(string)) error {
	if err := check(cfg); err != nil {
		return err
	}
	t, _ := targetOf(cfg)
	say("Testing the SSH connection to " + t.host + "…")
	if ok, detail := probe(ctx, t); !ok {
		return fmt.Errorf("Could not connect to %s over SSH: %s. Check Host, User and Key file in /settings under Terminal › SSH.", t.host, reason(detail))
	}
	say("Connected to " + t.host + " over SSH.")
	return nil
}

// Why a connection failed as the middle of a sentence: one line, no closing period, and words for no answer at all
func reason(detail string) string {
	detail = strings.TrimRight(strings.Join(strings.Fields(detail), " "), ".")
	if detail == "" {
		return "no answer came"
	}
	return detail
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
