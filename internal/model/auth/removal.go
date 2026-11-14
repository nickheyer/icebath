package auth

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
)

const ExternalLoginsNotAdoptedNotice = "External CLI logins (Codex CLI, Claude Code) are not adopted: auth.adopt_external_logins is false. " +
	"icebath uses only its own logins; run `icebath auth add <provider>` to add one."

func (s *Store) adoptExternalLogins(ctx context.Context) bool {
	cfg, err := s.cfg.LoadReadonly(ctx)
	if err != nil {
		return true
	}
	if cfg.Auth.AdoptExternalLogins {
		return true
	}
	s.mu.Lock()
	first := !s.noticed
	s.noticed = true
	s.mu.Unlock()
	if first {
		slog.Info(ExternalLoginsNotAdoptedNotice)
	}
	return false
}

func (s *Store) AdoptExternalLogins(ctx context.Context) bool {
	return s.adoptExternalLogins(s.bind(ctx))
}

type Removal struct {
	Entry    Entry
	Cleaned  []string
	Hints    []string
	Suppress bool
}

type removalStep struct {
	provider string
	source   string
	match    func(source string) bool
	remove   func(ctx context.Context, s *Store, provider string, removed *Entry) (Removal, error)
}

func (r removalStep) matches(provider, source string) bool {
	if r.provider != "*" && r.provider != provider {
		return false
	}
	if r.match != nil {
		return r.match(source)
	}
	return source == r.source
}

func removeEnvSource(ctx context.Context, s *Store, provider string, removed *Entry) (Removal, error) {
	result := Removal{Suppress: true}
	name := strings.TrimPrefix(removed.Source, "env:")
	if name == "" {
		return result, nil
	}
	inProcess := s.lookupEnv(ctx, name) != ""
	inFile := false
	if names, err := s.env.FileNames(ctx); err == nil {
		inFile = slices.Contains(names, name)
	}
	gone, err := s.env.Remove(ctx, name)
	if err != nil {
		return result, err
	}
	if gone {
		result.Cleaned = append(result.Cleaned, fmt.Sprintf("Cleared %s from .env", name))
	}
	if inProcess && !inFile {
		result.Hints = append(result.Hints,
			fmt.Sprintf("Note: %s is still set in your shell environment (not in ~/.icebath/.env).", name),
			"  Unset it there (shell profile, systemd EnvironmentFile, launchd plist, etc.) or it will keep being visible to icebath.",
			fmt.Sprintf("  The pool entry is now suppressed — icebath will ignore %s until you run `icebath auth add %s`.", name, provider))
	} else {
		result.Hints = append(result.Hints,
			fmt.Sprintf("Suppressed env:%s — it will not be re-seeded even if the variable is re-exported later.", name))
	}
	return result, nil
}

func removeIcebathPKCE(_ context.Context, s *Store, _ string, _ *Entry) (Removal, error) {
	result := Removal{Suppress: true}
	path := home.AnthropicOAuth.In(s.home)
	if _, err := os.Stat(path); err != nil {
		return result, nil
	}
	if err := os.Remove(path); err != nil {
		result.Hints = append(result.Hints, fmt.Sprintf("Could not delete %s: %v", path, err))
		return result, nil
	}
	result.Cleaned = append(result.Cleaned, "Cleared icebath Anthropic OAuth credentials")
	return result, nil
}

func removeAuthStoreOAuth(ctx context.Context, s *Store, provider string, _ *Entry) (Removal, error) {
	result := Removal{Suppress: true}
	ctx, unlock, err := s.lock(ctx, s.authPath, LockTimeout)
	if err != nil {
		return result, err
	}
	defer unlock()
	store, err := s.load(s.authPath)
	if err != nil {
		return result, err
	}
	if store.obj("providers").del(provider) {
		if err := s.save(ctx, store, s.authPath); err != nil {
			return result, err
		}
		result.Cleaned = append(result.Cleaned, fmt.Sprintf("Cleared %s OAuth tokens from auth store", provider))
	}
	return result, nil
}

func removeXAIDeviceCode(ctx context.Context, s *Store, provider string, removed *Entry) (Removal, error) {
	result, err := removeAuthStoreOAuth(ctx, s, provider, removed)
	result.Hints = append(result.Hints, "Run `icebath model` → xAI Grok OAuth (SuperGrok / Premium+) to re-authenticate if needed.")
	return result, err
}

func removeCodexDeviceCode(ctx context.Context, s *Store, provider string, removed *Entry) (Removal, error) {
	result, err := removeAuthStoreOAuth(ctx, s, provider, removed)
	if err != nil {
		return result, err
	}
	if err := s.Suppress(ctx, provider, "device_code"); err != nil {
		return result, err
	}
	result.Hints = append(result.Hints,
		"Suppressed openai-codex device_code source — it will not be re-seeded.",
		"Note: Codex CLI credentials still live in ~/.codex/auth.json",
		"Run `icebath auth add openai-codex` to re-enable if needed.")
	return result, nil
}

func suppressOnly(hints ...string) func(ctx context.Context, s *Store, provider string, removed *Entry) (Removal, error) {
	return func(_ context.Context, _ *Store, _ string, removed *Entry) (Removal, error) {
		result := Removal{Suppress: true}
		for _, h := range hints {
			result.Hints = append(result.Hints, strings.ReplaceAll(h, "{source}", removed.Source))
		}
		return result, nil
	}
}

var removalRegistry = []removalStep{
	{provider: "*", source: "env:", match: func(src string) bool { return strings.HasPrefix(src, "env:") }, remove: removeEnvSource},
	{provider: ProviderAnthropic, source: "claude_code", remove: suppressOnly(
		"Suppressed claude_code credential — it will not be re-seeded.",
		"Note: Claude Code credentials still live in ~/.claude/.credentials.json",
		"Run `icebath auth add anthropic` to re-enable if needed.")},
	{provider: ProviderAnthropic, source: sourceIcebathPKCE, remove: removeIcebathPKCE},
	{provider: ProviderPortal, source: "device_code", remove: removeAuthStoreOAuth},
	{provider: ProviderCodex, source: "device_code",
		match:  func(src string) bool { return src == "device_code" || strings.HasSuffix(src, ":device_code") },
		remove: removeCodexDeviceCode},
	{provider: ProviderXAIOAuth, source: "device_code", remove: removeXAIDeviceCode},
	{provider: ProviderQwen, source: "qwen-cli", remove: suppressOnly(
		"Suppressed qwen-cli credential — it will not be re-seeded.",
		"Note: Qwen CLI credentials still live in ~/.qwen/oauth_creds.json",
		"Run `icebath auth add qwen-oauth` to re-enable if needed.")},
	{provider: ProviderMiniMax, source: "oauth", remove: removeAuthStoreOAuth},
	{provider: "*", source: "config:",
		match: func(src string) bool { return strings.HasPrefix(src, "config:") || src == "model_config" },
		remove: suppressOnly(
			"Suppressed {source} — it will not be re-seeded.",
			"Note: The underlying value in config.yaml is unchanged.  Edit it directly if you want to remove the credential from disk.")},
}

func (s *Store) findRemovalStep(ctx context.Context, provider, source string) (removalStep, bool) {
	if spec, ok := s.dir.Spec(ctx, provider); ok && spec.RemoveSource != nil {
		if sources, hints, claimed := spec.RemoveSource(source); claimed {
			return removalStep{provider: provider, source: source,
				remove: func(ctx context.Context, s *Store, provider string, _ *Entry) (Removal, error) {
					for _, variant := range sources {
						if err := s.Suppress(ctx, provider, variant); err != nil {
							return Removal{Suppress: true}, err
						}
					}
					return Removal{Suppress: true, Hints: slices.Clone(hints)}, nil
				}}, true
		}
	}
	for _, step := range removalRegistry {
		if step.matches(provider, source) {
			return step, true
		}
	}
	return removalStep{}, false
}
