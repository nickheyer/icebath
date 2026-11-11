package tuigateway

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Active skin name and its user file mtime, nil mtime for a built in skin
type SkinSignature struct {
	Name  string
	Mtime *float64
}

// Skin baselines of the change watcher, the launch home's and each multiplexed profile home's
var skinBaselines struct {
	sync.Mutex
	last   *SkinSignature
	byHome map[string]SkinSignature
}

// Skin event payload for the display.skin of a config, empty when the skin engine fails
func SkinPayload(ctx context.Context, cfg map[string]any) (payload map[string]any) {
	defer func() {
		if recover() != nil {
			payload = map[string]any{}
		}
	}()
	config.InitSkinFromConfig(ctx, cfg)
	skin := config.GetActiveSkin(ctx)
	if skin == nil {
		return map[string]any{}
	}
	helpHeader := any("")
	if value, ok := skin.Branding["help_header"]; ok {
		helpHeader = value
	}
	return map[string]any{
		"name": skin.Name, "colors": skin.Colors,
		"light_colors": skin.LightColors, "dark_colors": skin.DarkColors,
		"branding": skin.Branding, "banner_logo": skin.BannerLogo,
		"banner_hero": skin.BannerHero, "tool_prefix": skin.ToolPrefix,
		"help_header": helpHeader, "customCSS": skin.CustomCSS,
	}
}

// Active skin signature of the context profile
func SkinSig(ctx context.Context) SkinSignature {
	display, _ := loadCfg(ctx)["display"].(map[string]any)
	name := "default"
	if value := display["skin"]; pyrepr.Truthy(value) {
		name = pyrepr.Str(value)
	}
	info, err := os.Stat(filepath.Join(home.Dir(ctx), "skins", name+".yaml"))
	if err != nil {
		return SkinSignature{Name: name}
	}
	mtime := float64(info.ModTime().UnixNano()) / 1e9
	return SkinSignature{Name: name, Mtime: &mtime}
}

// Profile home a peer's skin follows, empty for the launch home
func SkinHomeFor(profileID string) string {
	if !filepath.IsAbs(profileID) {
		return ""
	}
	info, err := os.Stat(profileID)
	if err != nil || !info.IsDir() {
		return ""
	}
	resolved, err := pypath.Resolve(profileID)
	if err != nil {
		return ""
	}
	launch, err := pypath.Resolve(home.Process())
	if err != nil || resolved == launch {
		return ""
	}
	return profileID
}

// Syncs the watcher baseline after a skin broadcast so the watcher does not repaint it again
func NoteSkinBroadcast(ctx context.Context) {
	sig := SkinSig(ctx)
	skinBaselines.Lock()
	defer skinBaselines.Unlock()
	if profileHome := SkinHomeFor(home.Override(ctx)); profileHome != "" {
		if skinBaselines.byHome == nil {
			skinBaselines.byHome = map[string]SkinSignature{}
		}
		skinBaselines.byHome[profileHome] = sig
		return
	}
	skinBaselines.last = &sig
}
