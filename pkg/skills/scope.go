package skills

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Scope binds one home, config, platform, and working directory for every skills operation
type Scope struct {
	Home     string
	Config   config.Tree
	Secrets  *config.Secrets
	Platform string
	Cwd      string
	Log      *slog.Logger
	// Overrides the home skills directory when set
	SkillsDirOverride string
}

// Scope over a home and its config with the process environment supplying platform and cwd
func NewScope(h string, cfg config.Tree) *Scope {
	if cfg == nil {
		cfg = config.Defaults()
	}
	cwd, _ := os.Getwd()
	return &Scope{Home: h, Config: cfg, Secrets: config.NewSecrets(h), Platform: envPlatform(), Cwd: cwd, Log: slog.Default()}
}

// Scope derived from a tool environment
func ScopeFromEnv(env *tools.Env) *Scope {
	if env == nil {
		return NewScope(home.Process(), nil)
	}
	s := &Scope{Home: env.Home, Config: env.Config, Secrets: env.Secrets, Platform: env.Platform, Cwd: env.Cwd, Log: env.Log}
	if s.Home == "" {
		s.Home = home.Process()
	}
	if s.Config == nil {
		s.Config = config.Defaults()
	}
	if s.Secrets == nil {
		s.Secrets = config.NewSecrets(s.Home)
	}
	if s.Platform == "" {
		s.Platform = envPlatform()
	}
	if s.Cwd == "" {
		s.Cwd, _ = os.Getwd()
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	return s
}

// Session platform from the process environment when the host did not set one
func envPlatform() string {
	if p := strings.TrimSpace(os.Getenv("ICEBATH_PLATFORM")); p != "" {
		return p
	}
	return strings.TrimSpace(os.Getenv("ICEBATH_SESSION_PLATFORM"))
}

// Active skills directory of the scope
func (s *Scope) SkillsDir() string {
	if s.SkillsDirOverride != "" {
		return s.SkillsDirOverride
	}
	return home.SkillsDir(s.Home)
}

// Logger of the scope
func (s *Scope) logger() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// Skills section of the config, nil when absent or malformed
func (s *Scope) skillsCfg() map[string]any {
	if s.Config == nil {
		return nil
	}
	return config.AsMap(s.Config["skills"])
}

// One skills config value, nil when the section or key is absent
func (s *Scope) skillsCfgGet(key string) any {
	cfg := s.skillsCfg()
	if cfg == nil {
		return nil
	}
	return cfg[key]
}

// Boolean skills config value with a default
func (s *Scope) skillsCfgBool(key string, fallback bool) bool {
	v := s.skillsCfgGet(key)
	if v == nil {
		return fallback
	}
	if b, ok := config.AsBool(v); ok {
		return b
	}
	return fallback
}

// Integer skills config value with a default
func (s *Scope) skillsCfgInt(key string, fallback int) int {
	v := s.skillsCfgGet(key)
	if v == nil {
		return fallback
	}
	if i, ok := config.AsInt(v); ok {
		return i
	}
	return fallback
}

// Display form of the home with the user directory shortened to a tilde
func (s *Scope) displayHome() string {
	return home.Display(s.Home)
}

// Expands a tilde and environment references in a config path entry
func expandPath(entry string) string {
	entry = os.ExpandEnv(entry)
	if entry == "~" || strings.HasPrefix(entry, "~/") || strings.HasPrefix(entry, `~\`) {
		if u, err := os.UserHomeDir(); err == nil {
			entry = u + entry[1:]
		}
	}
	return entry
}

// Anchors a relative config path at the home, absolute paths passing through
func (s *Scope) homeRelative(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(s.Home, p)
}

// Encodes a tool payload as the JSON string handed to the model
func jsonResult(payload map[string]any) string {
	b, err := json.Marshal(payload)
	if err != nil {
		return tools.ErrorResult(err)
	}
	return string(b)
}

// Failure payload with an error message and extra fields
func failResult(message string, extra map[string]any) string {
	payload := map[string]any{"success": false, "error": message}
	for k, v := range extra {
		payload[k] = v
	}
	return jsonResult(payload)
}
