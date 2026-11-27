package generate

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/generation"
	xaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/xai/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/strs"
)

// xAI's images and videos, with the live list of its picture models cached per address and key
type Services struct {
	providers *provider.Registry
	creds     *credentials.Registry
	mu        sync.Mutex
	live      map[string]catalog
}

func Register(registry *generation.Registry, providers *provider.Registry, creds *credentials.Registry) *Services {
	s := &Services{providers: providers, creds: creds, live: map[string]catalog{}}
	registry.Register(s)
	return s
}

func (s *Services) Services() []generation.Service {
	return []generation.Service{{
		Name:  xaiconfig.Name,
		Title: "xAI",
		Ready: func(cfg *config.Config) bool { _, ok := s.bearer(cfg); return ok },
		Setup: generation.Setup{Badge: "paid", Provider: web.XAIOAuth, Link: "https://console.x.ai/"},
		Images: &generation.Images{
			About:    "Grok images and edits, needs an xAI API key or a Grok sign-in",
			Settings: []string{xaiconfig.ImageModel, xaiconfig.Resolution, xaiconfig.ImageKey + xaiconfig.StorageEnabled, xaiconfig.ImageKey + xaiconfig.StoragePublic, xaiconfig.ImageKey + xaiconfig.StorageExpires},
			Model:    xaiconfig.ImageModel,
			Models:   s.imageModels,
			Default: func(ctx context.Context, cfg *config.Config) string {
				return s.catalog(ctx, cfg).order[0]
			},
			Takes: func(cfg *config.Config) generation.ImageTakes {
				return generation.ImageTakes{Edits: true, References: maxSources, Notes: notes(storageOf(cfg, xaiconfig.ImageKey).notice("image_gen"))}
			},
			Generate: s.draw,
		},
		Videos: &generation.Videos{
			About:    "Grok videos you can edit and extend, needs an xAI key or sign-in",
			Settings: []string{xaiconfig.VideoKey + xaiconfig.StorageEnabled, xaiconfig.VideoKey + xaiconfig.StoragePublic, xaiconfig.VideoKey + xaiconfig.StorageExpires},
			Models:   func(context.Context, *config.Config) []generation.Model { return videoModels },
			Default:  func(context.Context, *config.Config) string { return textVideoModel },
			Takes: func(cfg *config.Config) generation.VideoTakes {
				lines := []string{"chaining: for edit/extend pass the public HTTPS MP4 in `video` or `public_url` from the prior Imagine result (files-cdn). For " +
					"image-to-video / reference-to-video pass public image URLs the same way"}
				if notice := storageOf(cfg, xaiconfig.VideoKey).notice("video_gen"); notice != "" {
					lines = append(lines, "storage: "+notice)
				}
				return generation.VideoTakes{Modalities: []string{"text", "image"}, Aspects: videoAspects, Resolutions: videoResolutions, MinDuration: 1,
					MaxDuration: 15, Seed: true, References: maxVideoReferences, Notes: lines}
			},
			Generate: s.Generate,
		},
	}}
}

// Bearer of direct calls to xAI, the Grok sign-in first
func (s *Services) bearer(cfg *config.Config) (web.XAI, bool) {
	return web.XAIBearer(s.providers, s.creds, cfg, false)
}

// Whether the profile has xAI's key or Grok sign-in
func (s *Services) Ready(cfg *config.Config) bool {
	_, ok := s.bearer(cfg)
	return ok
}

// Headers of a call to xAI
func headers(key string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + key, "Content-Type": "application/json", "User-Agent": generation.Agent}
}

// Longest xAI keeps a stored file, and how long when the expiry setting names no number
const (
	maxExpiry  = 30 * 24 * 60 * 60
	safeExpiry = 2 * 24 * 60 * 60
)

// How xAI keeps what it makes of a kind
type storage struct {
	enabled, public bool
	// Seconds it keeps them, zero for until they are deleted
	expires int
}

// Storage settings of pictures or videos
func storageOf(cfg *config.Config, key string) storage {
	return storage{enabled: cfg.GetBool(key + xaiconfig.StorageEnabled), public: cfg.GetBool(key + xaiconfig.StoragePublic),
		expires: expiry(cfg.GetString(key + xaiconfig.StorageExpires))}
}

// Seconds of an expiry setting: zero for none, never or permanent, at most thirty days, two days for text that names no number
func expiry(text string) int {
	switch text = strings.ToLower(strings.TrimSpace(text)); text {
	case "", "default", "none", "null", "never", "permanent", "forever", "0":
		return 0
	}
	f, err := strconv.ParseFloat(text, 64)
	switch {
	case err != nil || math.IsNaN(f):
		return safeExpiry
	case f < 1:
		return 0
	case f >= maxExpiry:
		return maxExpiry
	}
	return int(f)
}

// storage_options of a request, nil when storage is off
func (st storage) options(prefix, extension string) map[string]any {
	if !st.enabled {
		return nil
	}
	options := map[string]any{"filename": fmt.Sprintf("%s-%s-%s.%s", prefix, time.Now().UTC().Format("20060102-150405"), strs.Random(4), extension),
		"public_url": st.public}
	if st.expires > 0 {
		options["expires_after"] = st.expires
	}
	return options
}

// Notice of what storage means, empty when it is off
func (st storage) notice(section string) string {
	if !st.enabled {
		return ""
	}
	retention := "without an automatic expiry"
	if st.expires > 0 {
		days := float64(st.expires) / (24 * 60 * 60)
		plural := "s"
		if days == 1 {
			plural = ""
		}
		retention = fmt.Sprintf("for about %s day%s", strconv.FormatFloat(days, 'g', -1, 64), plural)
	}
	return fmt.Sprintf("xAI Imagine storage is enabled so generated media gets a reusable public URL %s. xAI may bill for stored files and public URL hosting. "+
		"Disable this with `%s.xai.storage.enabled: false` or set `expires_after` to change the retention.", retention, section)
}

// The notice the first time the profile stores media of the section, empty after that
func (st storage) once(cfg *config.Config, section string) string {
	notice := st.notice(section)
	if notice == "" {
		return ""
	}
	dir := filepath.Join(cfg.ProfileDir(cfg.Profile()), "state")
	if os.MkdirAll(dir, 0o755) != nil {
		return notice
	}
	marker := filepath.Join(dir, section+"_xai_storage_notice_seen")
	if _, err := os.Stat(marker); err == nil {
		return ""
	}
	os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
	return notice
}

// The notice as lines, none when it is empty
func notes(notice string) []string {
	if notice == "" {
		return nil
	}
	return []string{notice}
}

// Keys of file_output an answer's extras carry over
var fileOutput = []string{"filename", "expires_at", "public_url_expires_at", "public_url_error", "storage_error"}
