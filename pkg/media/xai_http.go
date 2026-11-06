package media

import (
	"fmt"
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
