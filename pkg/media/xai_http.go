package media

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Default xAI REST origin
const DefaultXAIBaseURL = "https://api.x.ai/v1"

const maxXAIStorageExpiresAfter = 30 * 24 * 60 * 60
const safeXAIStorageExpiresAfter = 2 * 24 * 60 * 60

// Resolved bearer for direct xAI HTTP endpoints
type xaiCredentials struct {
	Provider string
	APIKey   string
	BaseURL  string
}

// Reports whether an xAI base URL override keeps the api.x.ai origin or a loopback host
func validateXAIBaseURL(override, fallback string) string {
	override = strings.TrimRight(strings.TrimSpace(override), "/")
	if override == "" {
		return fallback
	}
	u, err := url.Parse(override)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return fallback
	}
	host := strings.ToLower(u.Hostname())
	if host == "api.x.ai" || strings.HasSuffix(host, ".x.ai") || host == "localhost" || host == "127.0.0.1" {
		return override
	}
	return fallback
}

// xAI base URL override from the env, empty when unset
func xaiBaseURLOverride(env *tools.Env) string {
	return strings.TrimRight(firstSecret(env, "ICEBATH_XAI_BASE_URL", "XAI_BASE_URL"), "/")
}

// Reports whether xAI credentials are likely usable without any network
func hasXAICredentials(env *tools.Env) bool {
	if secret(env, "XAI_API_KEY") != "" {
		return true
	}
	state, _ := providerAuthState(env, "xai-oauth")
	tokens, _ := state["tokens"].(map[string]any)
	if stripAt(tokens, "access_token") != "" {
		return true
	}
	for _, row := range credentialPool(env, "xai-oauth") {
		if stripAt(row, "access_token") != "" {
			return true
		}
	}
	return false
}

// Bearer credentials for direct xAI endpoints, OAuth first unless an API key is preferred
func resolveXAIHTTPCredentials(env *tools.Env, preferAPIKey bool) xaiCredentials {
	explicit := secret(env, "XAI_API_KEY")
	if preferAPIKey && explicit != "" {
		return xaiCredentials{Provider: "xai", APIKey: explicit, BaseURL: validateXAIBaseURL(xaiBaseURLOverride(env), DefaultXAIBaseURL)}
	}
	for _, row := range credentialPool(env, "xai-oauth") {
		token := stripAt(row, "runtime_api_key")
		if token == "" {
			token = stripAt(row, "access_token")
		}
		if token == "" {
			continue
		}
		fallback := strings.TrimRight(stripAt(row, "runtime_base_url"), "/")
		if fallback == "" {
			fallback = strings.TrimRight(stripAt(row, "base_url"), "/")
		}
		if fallback == "" {
			fallback = DefaultXAIOAuthBaseURL
		}
		return xaiCredentials{Provider: "xai-oauth", APIKey: token, BaseURL: validateXAIBaseURL(xaiBaseURLOverride(env), fallback)}
	}
	state, _ := providerAuthState(env, "xai-oauth")
	tokens, _ := state["tokens"].(map[string]any)
	if token := stripAt(tokens, "access_token"); token != "" {
		fallback := strings.TrimRight(stripAt(state, "inference_base_url"), "/")
		if fallback == "" {
			fallback = DefaultXAIOAuthBaseURL
		}
		return xaiCredentials{Provider: "xai-oauth", APIKey: token, BaseURL: validateXAIBaseURL(xaiBaseURLOverride(env), fallback)}
	}
	base := strings.TrimRight(secret(env, "XAI_BASE_URL"), "/")
	if base == "" {
		base = DefaultXAIBaseURL
	}
	return xaiCredentials{Provider: "xai", APIKey: explicit, BaseURL: base}
}

// xAI Imagine storage settings of a config section
type xaiStorageConfig struct {
	Enabled      bool
	PublicURL    bool
	ExpiresAfter int
}

// Normalizes an xAI storage TTL, zero means permanent storage
func coerceExpiresAfter(v any) int {
	switch t := v.(type) {
	case nil:
		return 0
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		switch s {
		case "", "default", "none", "null", "never", "permanent", "forever", "0":
			return 0
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return safeXAIStorageExpiresAfter
		}
		if n <= 0 {
			return 0
		}
		return min(n, maxXAIStorageExpiresAfter)
	case bool:
		return safeXAIStorageExpiresAfter
	}
	f, ok := config.AsFloat(v)
	if !ok {
		return safeXAIStorageExpiresAfter
	}
	if int(f) <= 0 {
		return 0
	}
	return min(int(f), maxXAIStorageExpiresAfter)
}

// Storage settings of the image_gen or video_gen xai block, on by default
func readXAIStorageConfig(env *tools.Env, sectionName string) xaiStorageConfig {
	storage := section(section(section(cfgOf(env), sectionName), "xai"), "storage")
	return xaiStorageConfig{
		Enabled:      configBool(storage["enabled"], true),
		PublicURL:    configBool(storage["public_url"], true),
		ExpiresAfter: coerceExpiresAfter(storage["expires_after"]),
	}
}

// storage_options payload for an xAI request or nil when disabled
func buildXAIStorageOptions(env *tools.Env, sectionName, filenamePrefix, extension string) map[string]any {
	cfg := readXAIStorageConfig(env, sectionName)
	if !cfg.Enabled {
		return nil
	}
	ext := strings.TrimLeft(extension, ".")
	if ext == "" {
		ext = "bin"
	}
	filename := fmt.Sprintf("%s-%s-%s.%s", filenamePrefix, time.Now().UTC().Format("20060102-150405"), randomHex(4), ext)
	payload := map[string]any{"filename": filename, "public_url": cfg.PublicURL}
	if cfg.ExpiresAfter > 0 {
		payload["expires_after"] = cfg.ExpiresAfter
	}
	return payload
}

// User facing notice for xAI Imagine storage, empty when disabled
func xaiStorageNoticeText(env *tools.Env, sectionName string) string {
	cfg := readXAIStorageConfig(env, sectionName)
	if !cfg.Enabled {
		return ""
	}
	retention := "without an automatic expiry"
	if cfg.ExpiresAfter > 0 {
		days := float64(cfg.ExpiresAfter) / (24 * 60 * 60)
		plural := "s"
		if days == 1 {
			plural = ""
		}
		retention = fmt.Sprintf("for about %s day%s", strconv.FormatFloat(days, 'g', -1, 64), plural)
	}
	return fmt.Sprintf("xAI Imagine storage is enabled so generated media gets a reusable public URL %s. xAI may bill for stored files and public URL hosting. Disable this with `%s.xai.storage.enabled: false` or set `expires_after` to change the retention.", retention, sectionName)
}

// Storage notice once per home, marked seen afterwards
func maybeMarkXAIStorageNoticeSeen(env *tools.Env, sectionName string) string {
	notice := xaiStorageNoticeText(env, sectionName)
	if notice == "" {
		return ""
	}
	dir := filepath.Join(homeOf(env), "state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return notice
	}
	marker := filepath.Join(dir, sectionName+"_xai_storage_notice_seen")
	if _, err := os.Stat(marker); err == nil {
		return ""
	}
	os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
	return notice
}

// Headers for xAI JSON requests
func xaiJSONHeaders(apiKey string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + apiKey, "Content-Type": "application/json", "User-Agent": UserAgent}
}
