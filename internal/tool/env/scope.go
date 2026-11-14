package env

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
)

type PolicyUnavailableError struct {
	Reason string
	Err    error
}

func (e *PolicyUnavailableError) Error() string { return e.Reason }

func (e *PolicyUnavailableError) Unwrap() error { return e.Err }

type TerminalScope struct {
	values  map[string]string
	refused bool
	reason  string
}

func (t *TerminalScope) Value(name string) (string, bool) {
	if t == nil || t.refused {
		return "", false
	}
	v, ok := t.values[name]
	return v, ok
}

func (t *TerminalScope) Values() map[string]string {
	out := make(map[string]string, len(t.values))
	for k, v := range t.values {
		out[k] = v
	}
	return out
}

func (t *TerminalScope) Refused() (string, bool) {
	return t.reason, t.refused
}

func (t *TerminalScope) enforce() error {
	if t.refused {
		return &PolicyUnavailableError{Reason: "terminal policy unavailable for this profile: " + t.reason}
	}
	return nil
}

func NewTerminalScope(values map[string]string) *TerminalScope {
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	return &TerminalScope{values: out}
}

func RefusalScope(reason string) *TerminalScope {
	return &TerminalScope{values: map[string]string{}, refused: true, reason: reason}
}

type terminalScopeKey struct{}

func WithTerminalScope(ctx context.Context, sc *TerminalScope) context.Context {
	return context.WithValue(ctx, terminalScopeKey{}, sc)
}

func TerminalScopeOf(ctx context.Context) (*TerminalScope, bool) {
	sc, ok := ctx.Value(terminalScopeKey{}).(*TerminalScope)
	return sc, ok && sc != nil
}

func EnforceNoRefusal(ctx context.Context) error {
	if sc, ok := TerminalScopeOf(ctx); ok {
		return sc.enforce()
	}
	return nil
}

var toolLevelTerminalDefaults = map[string]string{
	"TERMINAL_SSH_HOST":                        "",
	"TERMINAL_SSH_USER":                        "",
	"TERMINAL_SSH_PORT":                        "22",
	"TERMINAL_SSH_KEY":                         "",
	"TERMINAL_DOCKER_ORPHAN_REAPER":            "True",
	"TERMINAL_DOCKER_PERSIST_ACROSS_PROCESSES": "True",
	"TERMINAL_SANDBOX_DIR":                     "",
	"TERMINAL_LIFETIME_SECONDS":                "300",
	"TERMINAL_DOCKER_SHARED_CONTAINER_KEY":     "",
}

func isNullNode(n *yaml.Node) bool {
	if n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n == nil || (n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null")
}

func isMappingNode(n *yaml.Node) bool {
	if n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n != nil && n.Kind == yaml.MappingNode
}

func mappingValue(n *yaml.Node, key string) (*yaml.Node, bool) {
	if !isMappingNode(n) {
		return nil, false
	}
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1], true
		}
	}
	return nil, false
}

func applyTerminalMapping(scope map[string]string, mapping *yaml.Node) {
	if !isMappingNode(mapping) {
		return
	}
	if mapping.Kind == yaml.AliasNode {
		mapping = mapping.Alias
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, value := mapping.Content[i].Value, mapping.Content[i+1]
		if isNullNode(value) {
			continue
		}
		text := config.TerminalEnvValue(value)
		if key == "cwd" && isCwdPlaceholder(strings.TrimSpace(text)) {
			continue
		}
		if name, ok := config.TerminalEnvVarForKey("terminal." + key); ok {
			scope[name] = text
		}
	}
}

func isCwdPlaceholder(v string) bool {
	switch v {
	case ".", "auto", "cwd":
		return true
	}
	return false
}

func DefaultTerminalScope() (*TerminalScope, error) {
	values, err := defaultTerminalValues()
	if err != nil {
		return nil, err
	}
	return &TerminalScope{values: values}, nil
}

func defaultTerminalValues() (map[string]string, error) {
	var n yaml.Node
	if err := n.Encode(config.Default().Terminal); err != nil {
		return nil, err
	}
	values := make(map[string]string, len(toolLevelTerminalDefaults))
	for k, v := range toolLevelTerminalDefaults {
		values[k] = v
	}
	applyTerminalMapping(values, &n)
	return values, nil
}

func (s *Settings) BuildProfileScope(ctx context.Context, profileHome string, overlay map[string]string) (*TerminalScope, error) {
	values, err := defaultTerminalValues()
	if err != nil {
		return nil, err
	}
	defaultImage, hasDefaultImage := values["TERMINAL_DOCKER_IMAGE"]
	hctx := home.WithOverride(ctx, profileHome)
	configPath, envPath, err := s.cfg.Paths(hctx)
	if err != nil {
		return nil, &PolicyUnavailableError{Reason: fmt.Sprintf("cannot resolve terminal config in %s: %v", profileHome, err), Err: err}
	}
	imagePinned := false
	if _, statErr := os.Stat(envPath); statErr == nil {
		readable := true
		if _, err := os.ReadFile(envPath); err != nil {
			_, pinned, perr := s.cfg.PinnedTerminalBackend(hctx)
			if !errors.Is(err, fs.ErrPermission) || perr != nil || !pinned {
				return nil, &PolicyUnavailableError{Reason: fmt.Sprintf("cannot read %s: %v", envPath, err), Err: err}
			}
			readable = false
		}
		if readable {
			profileEnv, err := s.cfg.Env().File(hctx)
			if err != nil {
				return nil, &PolicyUnavailableError{Reason: fmt.Sprintf("cannot read %s: %v", envPath, err), Err: err}
			}
			for k, v := range profileEnv {
				if strings.HasPrefix(k, "TERMINAL_") {
					values[k] = v
				}
			}
			_, imagePinned = profileEnv["TERMINAL_DOCKER_IMAGE"]
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return nil, &PolicyUnavailableError{Reason: fmt.Sprintf("cannot read %s: %v", envPath, statErr), Err: statErr}
	}
	if len(overlay) > 0 {
		for k, v := range overlay {
			if strings.HasPrefix(k, "TERMINAL_") && k != config.EnvTerminalImagePinned {
				values[k] = v
			}
		}
		if img, ok := overlay["TERMINAL_DOCKER_IMAGE"]; ok && (!hasDefaultImage || img != defaultImage) {
			imagePinned = true
		}
	}
	doc, err := s.cfg.RawDocumentReadonly(hctx)
	if err != nil {
		return nil, &PolicyUnavailableError{Reason: fmt.Sprintf("cannot resolve terminal config in %s: %v", profileHome, err), Err: err}
	}
	if rerr := doc.ReadError(); rerr != nil {
		return nil, &PolicyUnavailableError{Reason: fmt.Sprintf("cannot parse %s: %v", configPath, rerr), Err: rerr}
	}
	rawTerminal, _ := mappingValue(doc.Node(), "terminal")
	if isMappingNode(rawTerminal) {
		applyTerminalMapping(values, rawTerminal)
		if _, ok := mappingValue(rawTerminal, "docker_image"); ok {
			imagePinned = true
		}
	}
	managed, err := s.cfg.ApplyManagedOverlay(hctx, nil)
	if err != nil {
		return nil, &PolicyUnavailableError{Reason: fmt.Sprintf("cannot resolve terminal config in %s: %v", profileHome, err), Err: err}
	}
	managedTerminal, _ := mappingValue(managed.Node(), "terminal")
	if isMappingNode(managedTerminal) {
		base := config.NewYAMLDocument()
		if isMappingNode(rawTerminal) {
			if err := base.Set("terminal", rawTerminal); err != nil {
				return nil, err
			}
		} else if err := base.Set("terminal", map[string]any{}); err != nil {
			return nil, err
		}
		merged, err := s.cfg.ApplyManagedOverlay(hctx, base)
		if err != nil {
			return nil, &PolicyUnavailableError{Reason: fmt.Sprintf("cannot resolve terminal config in %s: %v", profileHome, err), Err: err}
		}
		mergedTerminal, _ := mappingValue(merged.Node(), "terminal")
		applyTerminalMapping(values, mergedTerminal)
		if img, ok := mappingValue(managedTerminal, "docker_image"); ok && !isNullNode(img) {
			imagePinned = true
		}
	}
	values[config.EnvTerminalImagePinned] = "0"
	if imagePinned {
		values[config.EnvTerminalImagePinned] = "1"
	}
	if values["TERMINAL_CWD"] == "" {
		backend := values["TERMINAL_ENV"]
		if backend == "" {
			backend = "local"
		}
		if strings.ToLower(strings.TrimSpace(backend)) == "local" {
			userHome, err := home.UserHome()
			if err != nil {
				return nil, err
			}
			if userHome != "" {
				values["TERMINAL_CWD"] = userHome
			}
		}
	}
	return &TerminalScope{values: values}, nil
}

func (s *Settings) PrepareProfileScope(ctx context.Context, profileHome string, overlay map[string]string) *TerminalScope {
	sc, err := s.BuildProfileScope(ctx, profileHome, overlay)
	if err != nil {
		slog.Warn(fmt.Sprintf("terminal policy unavailable: %v", err))
		return RefusalScope(err.Error())
	}
	return sc
}
