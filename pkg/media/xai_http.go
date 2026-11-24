package media

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/settings"
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

// Seconds xAI keeps a stored file by the text of a storage expiry setting, zero for permanent storage, the safe two days for text that names no number
func coerceExpiresAfter(text string) int {
	s := strings.ToLower(strings.TrimSpace(text))
	switch s {
	case "", "default", "none", "null", "never", "permanent", "forever", "0":
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	switch {
	case err != nil || math.IsNaN(f):
		return safeXAIStorageExpiresAfter
	case f < 1:
		return 0
	case f >= maxXAIStorageExpiresAfter:
		return maxXAIStorageExpiresAfter
	}
	return int(f)
}

// Storage settings of the image_gen or video_gen xai block, on by default
func readXAIStorageConfig(env *tools.Env, sectionName string) xaiStorageConfig {
	ctx := tools.WithEnvHome(context.Background(), env)
	enabled, publicURL, expires := settings.XAIImageStorage, settings.XAIImagePublicURL, settings.XAIImageExpiresAfter
	if sectionName == "video_gen" {
		enabled, publicURL, expires = settings.XAIVideoStorage, settings.XAIVideoPublicURL, settings.XAIVideoExpiresAfter
	}
	return xaiStorageConfig{
		Enabled:      enabled.Get(ctx),
		PublicURL:    publicURL.Get(ctx),
		ExpiresAfter: coerceExpiresAfter(expires.Get(ctx)),
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
