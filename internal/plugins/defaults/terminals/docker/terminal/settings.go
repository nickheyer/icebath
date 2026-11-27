package terminal

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	dockerconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminals/docker/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/terminal/sandbox"
)

// What one container is made with, read from the profile's settings
type options struct {
	image string
	// The image was chosen, so a container on another one is replaced rather than kept
	pinned bool
	cpu    float64
	memory int
	disk   int
	// /root and /workspace are folders of the host kept between runs
	persistent bool
	// Key of the environment without the profile, what the container is labelled for
	task    string
	volumes []string
	forward []string
	env     map[string]string
	network bool
	asUser  bool
	extra   []string
	// A container is found again and reused rather than made anew
	reuse  bool
	shm    string
	shared string
	snap   bool
	// The container serves one session only
	sessionScoped bool
}

// Options of a container for the environment kept under the task
func readOptions(cfg *config.Config, task string) (*options, error) {
	resources, err := terminal.SandboxResources(cfg)
	if err != nil {
		return nil, err
	}
	volumes, err := list(cfg, dockerconfig.Volumes)
	if err != nil {
		return nil, err
	}
	forward, err := list(cfg, dockerconfig.ForwardEnv)
	if err != nil {
		return nil, err
	}
	env, err := table(cfg, dockerconfig.Env)
	if err != nil {
		return nil, err
	}
	extra, err := list(cfg, dockerconfig.ExtraArgs)
	if err != nil {
		return nil, err
	}
	o := &options{
		image:      cmp.Or(strings.TrimSpace(cfg.GetString(dockerconfig.Image)), sandbox.Image),
		pinned:     pinned(cfg),
		cpu:        resources.CPU,
		memory:     resources.Memory,
		disk:       resources.Disk,
		persistent: resources.Persistent,
		task:       cmp.Or(task, "default"),
		volumes:    volumes,
		forward:    forwardNames(forward),
		env:        variables(env),
		network:    cfg.GetBool(dockerconfig.Network),
		asUser:     cfg.GetBool(dockerconfig.RunAsHostUser),
		extra:      extra,
		reuse:      cfg.GetBool(dockerconfig.PersistAcrossProcesses),
		shm:        strings.TrimSpace(cfg.GetString(dockerconfig.ShmSize)),
		shared:     strings.TrimSpace(cfg.GetString(dockerconfig.SharedContainerKey)),
		snap:       cfg.GetBool(dockerconfig.SnapCompat),
	}
	o.sessionScoped = !o.persistent && o.task != "default"
	if o.sessionScoped {
		o.reuse = false
	}
	return o, nil
}

// Whether the image setting was chosen, by the user or the variable that pins it, rather than left at icebath's default
func pinned(cfg *config.Config) bool {
	if cfg.GetBool(dockerconfig.ImagePinned) {
		return true
	}
	_, setting, ok := cfg.Setting(dockerconfig.Image)
	if !ok {
		return false
	}
	source, _ := cfg.Source(setting)
	return source != config.FromDefault
}

// Text items of a list setting: the list kept in the file, or the JSON list or the words its variable holds
func list(cfg *config.Config, key string) ([]string, error) {
	var items []any
	switch value := cfg.Get(key).(type) {
	case nil:
		return nil, nil
	case []string:
		return value, nil
	case []any:
		items = value
	case string:
		text := strings.TrimSpace(value)
		if !strings.HasPrefix(text, "[") {
			return strings.Fields(text), nil
		}
		if err := json.Unmarshal([]byte(text), &items); err != nil {
			return nil, malformed(cfg, key, value)
		}
	default:
		return nil, malformed(cfg, key, fmt.Sprint(value))
	}
	var out []string
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			slog.Warn("Ignoring non-string "+key+" entry", "entry", fmt.Sprint(item))
			continue
		}
		out = append(out, text)
	}
	return out, nil
}

// Table setting as names and values: the table kept in the file, which keeps its names in lower case, named here in upper case as
// variables are, or the JSON object its variable holds
func table(cfg *config.Config, key string) (map[string]any, error) {
	switch value := cfg.Get(key).(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		out := make(map[string]any, len(value))
		for name, item := range value {
			out[strings.ToUpper(name)] = item
		}
		return out, nil
	case string:
		out := map[string]any{}
		if strings.TrimSpace(value) == "" {
			return out, nil
		}
		if err := json.Unmarshal([]byte(value), &out); err != nil {
			return nil, malformed(cfg, key, value)
		}
		return out, nil
	default:
		return nil, malformed(cfg, key, fmt.Sprint(value))
	}
}

// Refusal of a list or table setting whose value is neither, naming it by its title
func malformed(cfg *config.Config, key, value string) error {
	title := key
	if _, setting, ok := cfg.Setting(key); ok && setting.Title != "" {
		title = setting.Title
	}
	return fmt.Errorf("%s is set to %q, which is not a list or a table. Change it in /settings under Terminal › Docker.", title, value)
}

// Forwarded variable names that are variable names, once each
func forwardNames(raw []string) []string {
	var out []string
	for _, entry := range raw {
		name := strings.TrimSpace(entry)
		switch {
		case name == "":
		case !terminal.VariableName(name):
			slog.Warn("Ignoring invalid "+dockerconfig.ForwardEnv+" entry", "entry", entry)
		case !slices.Contains(out, name):
			out = append(out, name)
		}
	}
	return out
}

// Variables set inside the container, those with names or values that can't be variables left out
func variables(raw map[string]any) map[string]string {
	out := map[string]string{}
	for key, value := range raw {
		name := strings.TrimSpace(key)
		if !terminal.VariableName(name) {
			slog.Warn("Ignoring invalid "+dockerconfig.Env+" key", "key", key)
			continue
		}
		switch v := value.(type) {
		case string:
			out[name] = v
		case bool, int, int64, float64, json.Number:
			out[name] = fmt.Sprint(v)
		default:
			slog.Warn("Ignoring non-string "+dockerconfig.Env+" value", "key", name, "type", fmt.Sprintf("%T", value))
		}
	}
	return out
}
