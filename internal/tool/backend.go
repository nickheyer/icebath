package tool

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/secret"
)

const ManagedProvider = "portal"

type Secrets struct {
	Scope       *secret.Scope
	Env         *config.Env
	Credentials Credentials
}

func (s Secrets) Credential(ctx context.Context, name string) (string, error) {
	if s.Scope == nil {
		return "", &UnfilledError{Name: "secret scope"}
	}
	v, err := s.Scope.Get(ctx, name, "")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(v), nil
}

func (s Secrets) EnvFile(ctx context.Context, name string) (string, error) {
	if s.Env == nil {
		return "", &UnfilledError{Name: "env file"}
	}
	v, _, err := s.Env.Get(ctx, name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(v), nil
}

func (s Secrets) Provider(ctx context.Context, envVar, provider string) (string, error) {
	if s.Credentials == nil {
		return "", &UnfilledError{Name: "credential store"}
	}
	if s.Scope == nil {
		return "", &UnfilledError{Name: "secret scope"}
	}
	suppressed := provider != "" && s.Credentials.Suppressed(ctx, provider, "env:"+envVar)
	if !suppressed {
		key, err := s.Credential(ctx, envVar)
		if err != nil {
			return "", err
		}
		if key != "" {
			return key, nil
		}
	}
	if s.Scope.Multiplexed(ctx) {
		return "", nil
	}
	key := ""
	if !suppressed {
		v, err := s.EnvFile(ctx, envVar)
		if err != nil {
			return "", err
		}
		key = v
	}
	if key != "" || provider == "" {
		return key, nil
	}
	for _, pool := range []string{provider, "custom:" + provider} {
		v, err := s.Credentials.Pooled(ctx, pool)
		if err != nil {
			slog.Debug(fmt.Sprintf("Could not read %s credential pool for %s: %v", provider, envVar, err))
			return "", nil
		}
		if v = strings.TrimSpace(v); v != "" {
			return v, nil
		}
	}
	return "", nil
}

func (s Secrets) OpenAIAudioKey(ctx context.Context) (string, error) {
	key, err := s.Provider(ctx, "VOICE_TOOLS_OPENAI_KEY", "")
	if err != nil || key != "" {
		return key, err
	}
	return s.Provider(ctx, "OPENAI_API_KEY", "openai-api")
}

func (s Secrets) FALKeyConfigured(ctx context.Context) (bool, error) {
	key, err := s.Credential(ctx, "FAL_KEY")
	if err != nil || key != "" {
		return key != "", err
	}
	key, err = s.EnvFile(ctx, "FAL_KEY")
	return key != "", err
}

func truthyNode(n *yaml.Node) bool {
	n = resolveNode(n)
	if n == nil {
		return false
	}
	if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
		return len(n.Content) > 0
	}
	var s config.Scalar
	if err := s.UnmarshalYAML(n); err != nil {
		return false
	}
	switch s.Kind() {
	case config.KindBool:
		v, _ := s.AsBool()
		return v
	case config.KindInt:
		v, _ := s.AsInt()
		return v != 0
	case config.KindFloat:
		v, _ := s.AsFloat()
		return v != 0
	case config.KindString:
		switch strings.ToLower(strings.TrimSpace(s.String())) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

func selectionNameKeys(section string) []string {
	switch section {
	case "browser":
		return []string{"cloud_provider"}
	case "web":
		return []string{"backend"}
	}
	return []string{"provider", "backend", "cloud_provider"}
}

func extraSelectionKeys(section string) []string {
	if section == "web" {
		return []string{"search_backend", "extract_backend"}
	}
	return nil
}

func SelectionKeys() []string {
	var out []string
	for _, section := range []string{"browser", "web", "image_gen", "video_gen", "tts", "stt"} {
		keys := append(append([]string{"use_gateway"}, selectionNameKeys(section)...), extraSelectionKeys(section)...)
		for _, k := range keys {
			out = append(out, section+"."+k)
		}
	}
	return sortedUnique(out)
}

func rawSection(ctx context.Context, st *config.Store, section string) (*yaml.Node, error) {
	if st == nil {
		return nil, &UnfilledError{Name: "config store"}
	}
	doc, err := st.RawDocumentReadonly(ctx)
	if err != nil {
		return nil, err
	}
	n := mapGet(doc.Node(), section)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, nil
	}
	return n, nil
}

func scalarText(n *yaml.Node) string {
	n = resolveNode(n)
	if n == nil || n.Kind != yaml.ScalarNode || isNullNode(n) {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(n.Value))
}

func ReadSelection(ctx context.Context, st *config.Store, section string) (string, error) {
	raw, err := rawSection(ctx, st, section)
	if err != nil || raw == nil {
		return "", err
	}
	return selectionOf(raw, section), nil
}

func selectionOf(raw *yaml.Node, section string) string {
	if truthyNode(mapGet(raw, "use_gateway")) {
		return ManagedProvider
	}
	for _, key := range selectionNameKeys(section) {
		if text := scalarText(mapGet(raw, key)); text != "" {
			return text
		}
	}
	return ""
}

func ReadWebSelection(ctx context.Context, st *config.Store, capability string) (string, error) {
	if capability != "" {
		raw, err := rawSection(ctx, st, "web")
		if err != nil {
			return "", err
		}
		if pin := scalarText(mapGet(raw, capability+"_backend")); pin != "" {
			return pin, nil
		}
	}
	return ReadSelection(ctx, st, "web")
}

func SelectionExists(ctx context.Context, st *config.Store, section string) (bool, error) {
	raw, err := rawSection(ctx, st, section)
	if err != nil || raw == nil {
		return false, err
	}
	if selectionOf(raw, section) != "" {
		return true, nil
	}
	for _, key := range extraSelectionKeys(section) {
		if n := resolveNode(mapGet(raw, key)); n != nil && n.Kind == yaml.ScalarNode && !isNullNode(n) && strings.TrimSpace(n.Value) != "" {
			return true, nil
		}
	}
	return false, nil
}

func PrefersGateway(ctx context.Context, st *config.Store, section string) (bool, error) {
	if st == nil {
		return false, &UnfilledError{Name: "config store"}
	}
	v, ok, err := st.Effective(ctx, config.JoinKeyPath(section, "use_gateway"))
	if err != nil || !ok {
		return false, err
	}
	return truthyNode(v.Value), nil
}

func removedBackends() map[string]map[string]string {
	return map[string]map[string]string{}
}

func RemovedBackend(section, name string) string {
	return removedBackends()[section][strings.ToLower(strings.Trim(strings.TrimSpace(name), `'"`))]
}

func SelectionError(section, selection, failure string) string {
	if note := RemovedBackend(section, selection); note != "" {
		failure = note
	}
	return fmt.Sprintf("%s is configured to use %s (set via icebath tools), but %s. Run 'icebath tools' to change it.", section, selection, failure)
}
