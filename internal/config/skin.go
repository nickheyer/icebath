package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/home"
)

const DefaultSkin = "default"

const skinCSSMax = 32768

type Skin struct {
	Name        string
	Description string
	Colors      Map[string]
	LightColors Map[string]
	DarkColors  Map[string]
	Spinner     SkinSpinner
	Branding    Map[string]
	ToolPrefix  string
	ToolEmojis  Map[string]
	BannerLogo  string
	BannerHero  string
	CustomCSS   string
}

type SkinSpinner struct {
	WaitingFaces  []string
	ThinkingFaces []string
	ThinkingVerbs []string
	Wings         [][2]string
}

type SkinEntry struct {
	Name        string
	Description string
	Source      SkinSource
}

type SkinSource string

const (
	SkinBuiltin SkinSource = "builtin"
	SkinUser    SkinSource = "user"
)

type SkinSignature struct {
	Name    string
	File    bool
	ModTime int64
}

type activeSkin struct {
	name string
	skin *Skin
}

func (k *Skin) Color(key, fallback string) string {
	if v, ok := k.Colors.Get(key); ok {
		return v
	}
	return fallback
}

func (k *Skin) Brand(key, fallback string) string {
	if v, ok := k.Branding.Get(key); ok {
		return v
	}
	return fallback
}

func (k *Skin) PromptSymbol(fallback string) string {
	symbol := k.Brand("prompt_symbol", fallback)
	if symbol == "" {
		symbol = fallback
	}
	if cleaned := strings.TrimSpace(symbol); cleaned != "" {
		return cleaned + " "
	}
	return strings.TrimSpace(fallback) + " "
}

func (k *Skin) StyleOverrides(color func(key, fallback string) string) Map[string] {
	if color == nil {
		color = k.Color
	}
	palette := map[string]string{}
	for _, e := range stylePalette {
		fallback := e[2]
		if strings.HasPrefix(fallback, "@") {
			fallback = palette[fallback[1:]]
		}
		palette[e[0]] = color(e[1], fallback)
	}
	title := "#FFD700"
	if v, ok := k.Colors.Get("banner_title"); ok {
		title = v
	}
	palette["badge_bg"] = title
	if v, ok := k.Colors.Get("status_bar_strong"); ok {
		palette["badge_bg"] = v
	}
	palette["badge_fg"] = "#1a1a2e"
	if v, ok := k.Colors.Get("status_bar_bg"); ok {
		palette["badge_fg"] = v
	}
	var out Map[string]
	for i := 0; i+1 < len(styleTemplates); i += 2 {
		tpl := styleTemplates[i+1]
		var b strings.Builder
		for {
			open := strings.IndexByte(tpl, '{')
			if open < 0 {
				b.WriteString(tpl)
				break
			}
			end := strings.IndexByte(tpl[open:], '}')
			b.WriteString(tpl[:open])
			b.WriteString(palette[tpl[open+1:open+end]])
			tpl = tpl[open+end+1:]
		}
		out.Set(styleTemplates[i], b.String())
	}
	return out
}

var stylePalette = [][3]string{
	{"prompt", "prompt", ""}, {"input_rule", "input_rule", "#CD7F32"},
	{"title", "banner_title", "#FFD700"}, {"text", "banner_text", "#FFF8DC"},
	{"dim", "banner_dim", "#555555"}, {"label", "ui_label", "@title"}, {"warn", "ui_warn", "#FF8C00"},
	{"error", "ui_error", "#FF6B6B"}, {"status_bg", "status_bar_bg", "#1a1a2e"},
	{"status_text", "status_bar_text", "@text"}, {"status_strong", "status_bar_strong", "@title"},
	{"status_dim", "status_bar_dim", "@dim"}, {"ok", "ui_ok", "#8FBC8F"},
	{"status_good", "status_bar_good", "@ok"}, {"status_warn", "status_bar_warn", "@warn"},
	{"accent", "banner_accent", "@warn"}, {"status_bad", "status_bar_bad", "@accent"},
	{"status_critical", "status_bar_critical", "@error"}, {"voice_bg", "voice_status_bg", "@status_bg"},
	{"menu_bg", "completion_menu_bg", "#1a1a2e"}, {"menu_current_bg", "completion_menu_current_bg", "#333355"},
	{"menu_meta_bg", "completion_menu_meta_bg", "@menu_bg"},
	{"menu_meta_current_bg", "completion_menu_meta_current_bg", "@menu_current_bg"},
}

var styleTemplates = []string{
	"input-area", "",
	"placeholder", "{dim} italic", "prompt", "{prompt}", "prompt-working", "{dim} italic",
	"hint", "{dim} italic",
	"status-bar", "bg:{status_bg} {status_text}", "status-bar-strong", "bg:{status_bg} {status_strong} bold",
	"status-bar-session-title", "bg:{badge_bg} {badge_fg} bold",
	"status-bar-dim", "bg:{status_bg} {status_dim}", "status-bar-good", "bg:{status_bg} {status_good} bold",
	"status-bar-warn", "bg:{status_bg} {status_warn} bold", "status-bar-bad", "bg:{status_bg} {status_bad} bold",
	"status-bar-critical", "bg:{status_bg} {status_critical} bold",
	"subagent-dock", "bg:{status_bg} {status_text}",
	"subagent-dock.heading", "bg:{status_bg} {status_strong} bold",
	"subagent-dock.selected", "bg:{menu_current_bg} {text} bold",
	"input-rule", "{input_rule}", "image-badge", "{label} bold",
	"completion-menu", "bg:{menu_bg} {text}", "completion-menu.completion", "bg:{menu_bg} {text}",
	"completion-menu.completion.current", "bg:{menu_current_bg} {title}",
	"completion-menu.meta.completion", "bg:{menu_meta_bg} {dim}",
	"completion-menu.meta.completion.current", "bg:{menu_meta_current_bg} {label}",
	"clarify-border", "{input_rule}", "clarify-title", "{title} bold", "clarify-question", "{text} bold",
	"clarify-choice", "{dim}", "clarify-selected", "{title} bold", "clarify-active-other", "{title} italic",
	"clarify-countdown", "{input_rule}",
	"sudo-prompt", "{error} bold", "sudo-border", "{input_rule}", "sudo-title", "{error} bold",
	"sudo-text", "{text}",
	"approval-border", "{input_rule}", "approval-title", "{warn} bold", "approval-desc", "{text} bold",
	"approval-cmd", "{dim} italic", "approval-choice", "{dim}", "approval-selected", "{title} bold",
	"voice-status", "bg:{voice_bg} {label}", "voice-status-recording", "bg:{voice_bg} {error} bold",
}

func skinField(m *yaml.Node, key string) (*yaml.Node, bool) {
	var found *yaml.Node
	for _, p := range mappingPairs(m) {
		if p.key.Value == key {
			found = resolveAlias(p.value)
		}
	}
	return found, found != nil
}

func skinText(m *yaml.Node, key, def string) string {
	v, ok := skinField(m, key)
	switch {
	case !ok:
		return def
	case isNull(v):
		return ""
	}
	return displayText(v)
}

func skinSection(m *yaml.Node, skin, key string) *yaml.Node {
	v, ok := skinField(m, key)
	switch {
	case !ok || isNull(v):
		return nil
	case v.Kind == yaml.MappingNode:
		return v
	}
	slog.Warn(fmt.Sprintf("Skin '%s' has invalid '%s' section type (%s); ignoring section", skin, key, typeName(v)))
	return nil
}

func overlayTexts(out *Map[string], section *yaml.Node) {
	for _, p := range mappingPairs(section) {
		v := resolveAlias(p.value)
		if v == nil || isNull(v) || v.Kind != yaml.ScalarNode {
			out.Delete(p.key.Value)
			continue
		}
		out.Set(p.key.Value, displayText(v))
	}
}

func skinItems(n *yaml.Node) []string {
	if n = resolveAlias(n); n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]string, 0, len(n.Content))
	for _, it := range n.Content {
		out = append(out, displayText(it))
	}
	return out
}

func skinWingPairs(n *yaml.Node) [][2]string {
	if n = resolveAlias(n); n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	var out [][2]string
	for _, it := range n.Content {
		if it = resolveAlias(it); it != nil && it.Kind == yaml.SequenceNode && len(it.Content) == 2 {
			out = append(out, [2]string{displayText(it.Content[0]), displayText(it.Content[1])})
		}
	}
	return out
}

func buildSkin(data *yaml.Node) *Skin {
	def, _ := builtinSkinNamed(DefaultSkin)
	defNode := def.node()
	name := skinText(data, "name", "unknown")
	merged := func(key string) Map[string] {
		var out Map[string]
		overlayTexts(&out, skinSection(defNode, def.name, key))
		overlayTexts(&out, skinSection(data, name, key))
		return out
	}
	only := func(key string) Map[string] {
		var out Map[string]
		overlayTexts(&out, skinSection(data, name, key))
		return out
	}
	k := &Skin{
		Name:        name,
		Description: skinText(data, "description", ""),
		Colors:      merged("colors"),
		LightColors: only("light_colors"),
		DarkColors:  only("dark_colors"),
		Branding:    merged("branding"),
		ToolPrefix:  skinText(data, "tool_prefix", skinText(defNode, "tool_prefix", "┊")),
		ToolEmojis:  only("tool_emojis"),
		BannerLogo:  skinText(data, "banner_logo", ""),
		BannerHero:  skinText(data, "banner_hero", ""),
	}
	spinner := skinSection(data, name, "spinner")
	k.Spinner = SkinSpinner{
		WaitingFaces:  skinItems(childOf(spinner, "waiting_faces")),
		ThinkingFaces: skinItems(childOf(spinner, "thinking_faces")),
		ThinkingVerbs: skinItems(childOf(spinner, "thinking_verbs")),
		Wings:         skinWingPairs(childOf(spinner, "wings")),
	}
	css := []rune(strings.TrimSpace(skinText(data, "customCSS", "")))
	if len(css) > skinCSSMax {
		css = css[:skinCSSMax]
	}
	k.CustomCSS = string(css)
	return k
}

func readSkinFile(path string) *yaml.Node {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to load skin from %s: %v", path, err))
		return nil
	}
	var root yaml.Node
	if err := LoadYAML(data, &root); err != nil {
		slog.Debug(fmt.Sprintf("Failed to load skin from %s: %v", path, err))
		return nil
	}
	if root.Kind != yaml.MappingNode {
		return nil
	}
	if _, ok := skinField(&root, "name"); !ok {
		return nil
	}
	return &root
}

func (s *Store) skinsDir(ctx context.Context) (string, error) {
	h, err := s.homes.Home(ctx)
	if err != nil {
		return "", err
	}
	return home.Skins.In(h), nil
}

func (s *Store) ListSkins(ctx context.Context) ([]SkinEntry, error) {
	dir, err := s.skinsDir(ctx)
	if err != nil {
		return nil, err
	}
	var out []SkinEntry
	for _, b := range builtinSkins() {
		out = append(out, SkinEntry{Name: b.name, Description: b.description, Source: SkinBuiltin})
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out, nil
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(skinFileKey(e.Name()), ".yaml") {
			names = append(names, e.Name())
		}
	}
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(skinFileKey(a), skinFileKey(b)) })
	for _, n := range names {
		data := readSkinFile(filepath.Join(dir, n))
		if data == nil {
			continue
		}
		name := skinText(data, "name", strings.TrimSuffix(n, filepath.Ext(n)))
		if slices.ContainsFunc(out, func(e SkinEntry) bool { return e.Name == name }) {
			continue
		}
		out = append(out, SkinEntry{Name: name, Description: skinText(data, "description", ""), Source: SkinUser})
	}
	return out, nil
}

func (s *Store) LoadSkin(ctx context.Context, name string) (*Skin, error) {
	dir, err := s.skinsDir(ctx)
	if err != nil {
		return nil, err
	}
	var data *yaml.Node
	path := filepath.Join(dir, name+".yaml")
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
		data = readSkinFile(path)
	}
	if data != nil {
		return buildSkin(data), nil
	}
	b, ok := builtinSkinNamed(name)
	if !ok {
		slog.Warn(fmt.Sprintf("Skin '%s' not found, using default", name))
		b, _ = builtinSkinNamed(DefaultSkin)
	}
	return buildSkin(b.node()), nil
}

func (s *Store) skinKey(ctx context.Context) (string, error) {
	h, err := s.homes.Home(ctx)
	if err != nil {
		return "", err
	}
	return s.homeKey(h), nil
}

func (s *Store) SetActiveSkin(ctx context.Context, name string) (*Skin, error) {
	key, err := s.skinKey(ctx)
	if err != nil {
		return nil, err
	}
	k, err := s.LoadSkin(ctx, name)
	if err != nil {
		return nil, err
	}
	s.skinMu.Lock()
	defer s.skinMu.Unlock()
	if s.skins == nil {
		s.skins = map[string]activeSkin{}
	}
	s.skins[key] = activeSkin{name: name, skin: k}
	return k, nil
}

func (s *Store) ActiveSkin(ctx context.Context) (*Skin, error) {
	key, err := s.skinKey(ctx)
	if err != nil {
		return nil, err
	}
	s.skinMu.Lock()
	e, ok := s.skins[key]
	s.skinMu.Unlock()
	if ok {
		return e.skin, nil
	}
	name := DefaultSkin
	if cfg, err := s.LoadReadonly(ctx); err != nil {
		slog.Debug(fmt.Sprintf("Skin config read failed, using the default skin: %v", err))
	} else {
		name = configuredSkin(cfg)
	}
	return s.SetActiveSkin(ctx, name)
}

func (s *Store) ActiveSkinName(ctx context.Context) (string, error) {
	key, err := s.skinKey(ctx)
	if err != nil {
		return "", err
	}
	s.skinMu.Lock()
	defer s.skinMu.Unlock()
	if e, ok := s.skins[key]; ok {
		return e.name, nil
	}
	return DefaultSkin, nil
}

func (s *Store) RefreshSkin(ctx context.Context) (*Skin, error) {
	cfg, err := s.LoadReadonly(ctx)
	if err != nil {
		return nil, err
	}
	return s.SetActiveSkin(ctx, configuredSkin(cfg))
}

func configuredSkin(cfg *Config) string {
	if name := strings.TrimSpace(cfg.Display.Skin); name != "" {
		return name
	}
	return DefaultSkin
}

func storedSkin(cfg *Config) string {
	if cfg.Display.Skin != "" {
		return cfg.Display.Skin
	}
	return DefaultSkin
}

func (s *Store) SkinSignature(ctx context.Context) (SkinSignature, error) {
	cfg, err := s.LoadReadonly(ctx)
	if err != nil {
		return SkinSignature{}, err
	}
	dir, err := s.skinsDir(ctx)
	if err != nil {
		return SkinSignature{}, err
	}
	sig := SkinSignature{Name: storedSkin(cfg)}
	if fi, err := os.Stat(filepath.Join(dir, sig.Name+".yaml")); err == nil {
		sig.File, sig.ModTime = true, fi.ModTime().UnixNano()
	}
	return sig, nil
}
