package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
)

func init() {
	observability.ConnectedPlatformsHook = connectedPlatformCount
	observability.RuntimeHomeOfSourceHook = runtimeHomeOfSource
	observability.SetCatalogSource("bundled_platform_names", bundledPlatformNames)
	observability.ClassifySendErrorHook = func(errText string) string { return ClassifySendError(nil, errText) }
	observability.GatewayAdapterPlatformsHook = platformMemberValues
	observability.PluginPlatformNamesHook = pluginPlatformNames
	observability.CatalogPlatformOwnerHook = catalogPlatformOwnerRow
	observability.ProcessStartTimeHook = GetProcessStartTime
	observability.RuntimeStatusPidIsLiveHook = RuntimeStatusPidIsLive
}

// Values of the declared Platform members, pseudo members excluded
func platformMemberValues() []string {
	members := PlatformMembers()
	out := make([]string, len(members))
	for i, p := range members {
		out[i] = string(p)
	}
	return out
}

// Names of the plugin registered platform entries of the scope bound to ctx
func pluginPlatformNames(ctx context.Context) []string {
	out := []string{}
	for _, e := range Registry.PluginEntries(ctx) {
		out = append(out, e.Name)
	}
	return out
}

// Raw catalog name the installer recorded for the plugin whose adapter factory code registers a platform in a home
func catalogPlatformOwnerRow(homeDir, platform string) (any, bool) {
	entry := Registry.Get(home.WithOverride(context.Background(), homeDir), platform)
	if entry == nil || entry.Source != "plugin" {
		return nil, false
	}
	pluginsDir, err := pypath.Resolve(filepath.Join(homeDir, "plugins"))
	if err != nil {
		return nil, false
	}
	codeFile, ok := callableFile(entry.AdapterFactory)
	if !ok {
		return nil, false
	}
	codePath, err := pypath.Resolve(codeFile)
	if err != nil {
		return nil, false
	}
	rel, err := filepath.Rel(pluginsDir, codePath)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.Dir(codePath) == pluginsDir {
		return nil, false
	}
	row, _ := plugins.ReadSidecarRows(pluginsDir)[strings.Split(rel, string(filepath.Separator))[0]].(map[string]any)
	if block, isDict := row["catalog"].(map[string]any); isDict {
		return block["name"], true
	}
	return row["catalog_name"], true
}

// Source file defining a Go function value, false for anything that is not a function
func callableFile(fn any) (string, bool) {
	v := reflect.ValueOf(fn)
	if !v.IsValid() || v.Kind() != reflect.Func || v.IsNil() {
		return "", false
	}
	f := runtime.FuncForPC(v.Pointer())
	if f == nil {
		return "", false
	}
	file, _ := f.FileLine(v.Pointer())
	return file, file != ""
}

// Number of messaging platforms the loaded gateway config connects
func connectedPlatformCount(ctx context.Context) (int, error) {
	cfg, err := LoadGatewayConfig(ctx)
	if err != nil {
		return 0, err
	}
	return len(cfg.GetConnectedPlatforms(ctx)), nil
}

// Swappable install root whose plugins/platforms tree lists the shipped messaging platforms
var bundledPlatformsRepoRoot = pm.RepoRoot

// Lowercased names of the shipped platform plugin directories that carry a plugin.yaml, empty when unreadable
func bundledPlatformNames() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(bundledPlatformsRepoRoot(), "plugins", "platforms"))
	if err != nil {
		return []string{}, nil
	}
	seen := map[string]bool{}
	names := []string{}
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(bundledPlatformsRepoRoot(), "plugins", "platforms", e.Name(), "plugin.yaml"))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if name := strings.ToLower(e.Name()); !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// Runtime profile home pinned on a message source's routing identity, false when none is pinned
func runtimeHomeOfSource(source any) (string, bool) {
	s, _ := source.(*SessionSource)
	if identity := IdentityOf(s); identity != nil {
		return identity.RuntimeHome, true
	}
	return "", false
}
