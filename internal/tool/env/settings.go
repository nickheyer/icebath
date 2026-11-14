package env

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
)

var _ TerminalSettings = (*Settings)(nil)

type bridgedValue struct {
	value   string
	base    string
	baseSet bool
}

type Settings struct {
	cfg       *config.Store
	homes     *home.Resolver
	providers *Providers

	mu        sync.Mutex
	attempted bool
	bridged   map[string]bridgedValue

	namesMu sync.Mutex
	names   map[[2]string]string
}

func NewSettings(cfg *config.Store, homes *home.Resolver, providers *Providers) (*Settings, error) {
	switch {
	case cfg == nil:
		return nil, &tool.UnfilledError{Name: "env.Settings config store"}
	case homes == nil:
		return nil, &tool.UnfilledError{Name: "env.Settings home resolver"}
	case providers == nil:
		return nil, &tool.UnfilledError{Name: "env.Providers"}
	}
	return &Settings{cfg: cfg, homes: homes, providers: providers, bridged: map[string]bridgedValue{}, names: map[[2]string]string{}}, nil
}

func (s *Settings) Lookup(ctx context.Context, name string) (string, bool, error) {
	if sc, ok := TerminalScopeOf(ctx); ok {
		if err := sc.enforce(); err != nil {
			return "", false, err
		}
		v, ok := sc.values[name]
		return v, ok, nil
	}
	pv, pok := os.LookupEnv(name)
	s.mu.Lock()
	b, ok := s.bridged[name]
	s.mu.Unlock()
	if ok && pok == b.baseSet && pv == b.base {
		return b.value, true, nil
	}
	return pv, pok, nil
}

func (s *Settings) TerminalEnv(ctx context.Context, name string) (string, error) {
	v, _, err := s.Lookup(ctx, name)
	return v, err
}

func (s *Settings) tenv(ctx context.Context, name, def string) (string, error) {
	v, ok, err := s.Lookup(ctx, name)
	if err != nil {
		return "", err
	}
	if !ok {
		return def, nil
	}
	return v, nil
}

func (s *Settings) tenvBool(ctx context.Context, name, def string) (bool, error) {
	v, err := s.tenv(ctx, name, def)
	if err != nil {
		return false, err
	}
	return truthyWord(v), nil
}

func truthyWord(v string) bool {
	switch strings.ToLower(v) {
	case "true", "1", "yes":
		return true
	}
	return false
}

type InvalidValueError struct {
	Name     string
	Raw      string
	Expected string
}

func (e *InvalidValueError) Error() string {
	return fmt.Sprintf("Invalid value for %s: '%s' (expected %s). Check ~/.icebath/.env or environment variables.", e.Name, e.Raw, e.Expected)
}

func (s *Settings) rawSetting(ctx context.Context, name, def string) (string, error) {
	if strings.HasPrefix(name, "TERMINAL_") {
		return s.tenv(ctx, name, def)
	}
	if v, ok := os.LookupEnv(name); ok {
		return v, nil
	}
	return def, nil
}

func (s *Settings) parseInt(ctx context.Context, name, def string) (int, error) {
	raw, err := s.rawSetting(ctx, name, def)
	if err != nil {
		return 0, err
	}
	n, ok := parseDecimalInt(raw)
	if !ok {
		return 0, &InvalidValueError{Name: name, Raw: raw, Expected: "integer"}
	}
	return n, nil
}

func (s *Settings) parseFloat(ctx context.Context, name, def string) (float64, error) {
	raw, err := s.rawSetting(ctx, name, def)
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, &InvalidValueError{Name: name, Raw: raw, Expected: "number"}
	}
	return f, nil
}

func parseDecimalInt(raw string) (int, bool) {
	text := strings.TrimSpace(raw)
	sign := ""
	if strings.HasPrefix(text, "+") || strings.HasPrefix(text, "-") {
		sign, text = text[:1], text[1:]
	}
	if text == "" || strings.HasPrefix(text, "_") || strings.HasSuffix(text, "_") || strings.Contains(text, "__") {
		return 0, false
	}
	digits := strings.ReplaceAll(text, "_", "")
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(sign + digits)
	return n, err == nil
}

func (s *Settings) ensureBridged(ctx context.Context) {
	if _, ok := TerminalScopeOf(ctx); ok {
		return
	}
	if home.Override(ctx) != "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempted {
		return
	}
	s.attempted = true
	names := append(config.TerminalEnvVars(), config.EnvTerminalImagePinned)
	target := config.EnvMap{}
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok {
			target[n] = v
		}
	}
	before := make(map[string]string, len(target))
	for k, v := range target {
		before[k] = v
	}
	raw, err := s.cfg.RawDocument(ctx)
	if err != nil {
		slog.Debug("terminal config → env fallback bridge failed", "err", err)
		return
	}
	override := false
	if node, ok := raw.Get("terminal"); ok && isMappingNode(node) {
		override = true
	} else if _, set := target["TERMINAL_ENV"]; set {
		return
	}
	if err := s.cfg.ApplyTerminalEnv(ctx, target, nil, &override); err != nil {
		slog.Debug("terminal config → env fallback bridge failed", "err", err)
	}
	for k, v := range target {
		if b, had := before[k]; !had || b != v {
			s.bridged[k] = bridgedValue{value: v, base: b, baseSet: had}
		}
	}
}

func (s *Settings) HomeProfileName(ctx context.Context) string {
	h, err := s.homes.Home(ctx)
	if err != nil {
		return "default"
	}
	root, err := home.DefaultRoot()
	if err != nil {
		return "default"
	}
	key := [2]string{h, root}
	s.namesMu.Lock()
	cached, ok := s.names[key]
	s.namesMu.Unlock()
	if ok {
		return cached
	}
	name := activeProfileName(h, root)
	if _, err := os.Stat(h); err == nil {
		s.namesMu.Lock()
		s.names[key] = name
		s.namesMu.Unlock()
	}
	return name
}

func activeProfileName(h, root string) string {
	resolved := resolvedPath(h)
	if resolved == resolvedPath(root) {
		return "default"
	}
	name := filepath.Base(resolved)
	if home.ValidProfileID(name) && resolvedPath(filepath.Dir(home.ProfileHome(root, name))) == filepath.Dir(resolved) {
		return name
	}
	return "custom"
}

func resolvedPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := resolveLoose(abs); err == nil {
		return real
	}
	return abs
}

func ProfileContainerKey(profile string) string {
	if profile == "default" {
		return "default"
	}
	return "profile:" + profile
}

func (s *Settings) safeGetwd(ctx context.Context) (string, error) {
	if wd, err := os.Getwd(); err == nil {
		return wd, nil
	}
	if v, err := s.tenv(ctx, "TERMINAL_CWD", ""); err != nil {
		return "", err
	} else if v != "" {
		return v, nil
	}
	return home.ExpandUser("~"), nil
}
