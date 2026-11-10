package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	daySeconds           = 86_400
	gib                  = 1024 * 1024 * 1024
	ramNominalFactor     = 1.1
	behindMaxAgeSeconds  = 7 * daySeconds
	localProviderUnknown = "no"
)

var (
	installVersionAgeThresholds = []threshold{{7 * daySeconds, "lt_7d"}, {30 * daySeconds, "7d_to_30d"}, {90 * daySeconds, "30d_to_90d"}}
	ramThresholds               = []threshold{{8, "lt_8g"}, {16, "8g_to_16g"}, {32, "16g_to_32g"}, {64, "32g_to_64g"}, {128, "64g_to_128g"}}
	releaseChannels             = map[string]string{"stable": "stable", "main": "main", "canary": "main"}
	mainBranches                = Set("main", "master")
	localProviderAliases        = Set("ollama", "local", "vllm", "llamacpp", "llama.cpp", "llama-cpp", "lmstudio")
	inheritsMain                = Set("", "auto", "main")
)

// Hooks the provider and selfupdate packages install from an init function
var (
	// IsLocalEndpointHook reports whether a base URL points at a local or private network server, set by pkg/provider
	IsLocalEndpointHook func(baseURL string) bool
	// NamedCustomProviderHook returns the providers or custom_providers entry a provider name resolves to, set by pkg/provider
	NamedCustomProviderHook func(ctx context.Context, name string) any
	// ResolveUpdateChannelHook returns the update channel an install follows, set by pkg/selfupdate
	ResolveUpdateChannelHook func(cfg map[string]any, projectRoot string) (string, error)
	// PackageChannelHook returns the channel a packaged build's install stamp bakes in, empty for a source install, set by pkg/selfupdate
	PackageChannelHook func(projectRoot string) (string, error)
)

var errHookUnavailable = errors.New("owning module unavailable")

// Readers tests replace, the installed version, the code root and the host RAM of this install
var (
	installVersionInfo   = updaterecord.GetVersionInfo
	installProjectRoot   = config.GetProjectRoot
	installRAMTotalBytes = hostinfo.RAMTotalBytes
)

// Install method the client resource names, the Microsoft Store build included
func DetectClientInstallMethod(ctx context.Context) (string, error) {
	root := installProjectRoot()
	if mechanism, ok := pm.ReadInstallStamp(root).Get("updateMechanism"); ok && mechanism == "microsoft-store" {
		return "microsoft-store", nil
	}
	return config.DetectInstallMethod(ctx, home.Dir(ctx), root)
}

// Channel this install follows from its packaged channel, channel record or checkout branch
func ReleaseChannel(cfg map[string]any) (string, error) {
	if ResolveUpdateChannelHook == nil || PackageChannelHook == nil {
		return "", errHookUnavailable
	}
	root := installProjectRoot()
	resolved, err := ResolveUpdateChannelHook(cfg, root)
	if err != nil {
		return "", err
	}
	packaged := ""
	if resolved == "main" {
		if packaged, err = PackageChannelHook(root); err != nil {
			return "", err
		}
	}
	if resolved != "main" || packaged != "" {
		if channel, ok := releaseChannels[resolved]; ok {
			return channel, nil
		}
		return "unknown", nil
	}
	info, err := installVersionInfo()
	if err != nil {
		return "", err
	}
	if info.Branch == nil || *info.Branch == "" {
		return "unknown", nil
	}
	if mainBranches.Has(*info.Branch) {
		return "main", nil
	}
	return "dev", nil
}

// Installed version age from its own commit date, zero now meaning the current time
func InstallVersionAgeBucket(now float64) (string, error) {
	info, err := installVersionInfo()
	if err != nil {
		return "", err
	}
	if info.CommitDate == nil || *info.CommitDate <= 0 {
		return "unknown", nil
	}
	if now == 0 {
		now = pyTime()
	}
	return bucket(math.Max(0, now-float64(*info.CommitDate)), installVersionAgeThresholds, "gte_90d"), nil
}

// Commits behind from the update check's fresh cached result for this exact revision
func BehindBucket(ctx context.Context, homeDir string, now float64) (string, error) {
	root := installProjectRoot()
	if homeDir == "" {
		homeDir = home.Dir(ctx)
	}
	cached, ok := readRecord(filepath.Join(homeDir, "source-checks", tools.InstallID(root)+".json"))
	if !ok {
		return "unknown", nil
	}
	var behind, head any
	if status, ok := asDict(cached.Vals["status"]); ok {
		behind = status["behind"]
	}
	if identity, ok := asDict(cached.Vals["identity"]); ok {
		head = identity["head"]
	}
	info, err := installVersionInfo()
	if err != nil {
		return "", err
	}
	if now == 0 {
		now = pyTime()
	}
	count, isInt := pyInt(behind)
	ts, isNumber := pyNumber(cached.Vals["ts"])
	if flag, isBool := cached.Vals["ts"].(bool); isBool {
		ts, isNumber = 0, true
		if flag {
			ts = 1
		}
	}
	commit := ""
	if info.Commit != nil {
		commit = *info.Commit
	}
	headText, _ := head.(string)
	if !isInt || count.Sign() < 0 || !isNumber || !(0 <= now-ts && now-ts < behindMaxAgeSeconds) ||
		(pyTruthy(head) && commit != "" && (headText == "" || headText != commit)) {
		return "unknown", nil
	}
	if !count.IsInt64() || count.Int64() > math.MaxInt32 {
		return CountBucket(math.MaxInt32), nil
	}
	return CountBucket(int(count.Int64())), nil
}

// Nominal RAM size bucket of the host, unknown when the OS does not say
func RamBucket() (string, error) {
	total := installRAMTotalBytes()
	if total <= 0 {
		return "unknown", nil
	}
	return bucket(float64(total)*ramNominalFactor/gib, ramThresholds, "gte_128g"), nil
}

// Host GPU vendor class within the contract vocabulary
func GpuClass() (string, error) {
	if value := hostinfo.GPUClass(); GPUClasses.Has(value) {
		return value, nil
	}
	return "unknown", nil
}

func isLocalEndpoint(baseURL any) (bool, error) {
	text, ok := baseURL.(string)
	if !ok || pystr.Strip(text) == "" {
		return false, nil
	}
	if IsLocalEndpointHook == nil {
		return false, errHookUnavailable
	}
	return IsLocalEndpointHook(text), nil
}

func customBaseURL(ctx context.Context, provider string) (any, error) {
	if NamedCustomProviderHook == nil {
		return nil, errHookUnavailable
	}
	if entry, ok := asDict(NamedCustomProviderHook(ctx, provider)); ok {
		return entry["base_url"], nil
	}
	return nil, nil
}

func installSub(cfg any, key string) any {
	if d, ok := asDict(cfg); ok {
		return d[key]
	}
	return nil
}

func slotIsLocal(ctx context.Context, slot any) (bool, error) {
	provider := norm(installSub(slot, "provider"))
	if localProviderAliases.Has(provider) {
		return true, nil
	}
	if local, err := isLocalEndpoint(installSub(slot, "base_url")); err != nil || local {
		return local, err
	}
	if catalogSafe("user_named_model_providers").Minus(Custom).Has(provider) {
		return true, nil
	}
	if inheritsMain.Has(provider) {
		return false, nil
	}
	base, err := customBaseURL(ctx, provider)
	if err != nil {
		return false, err
	}
	return isLocalEndpoint(base)
}

func auxSlotRoutesItself(slot map[string]any) bool {
	if !inheritsMain.Has(norm(slot["provider"])) {
		return true
	}
	for _, key := range []string{"base_url", "api_key"} {
		if text, ok := slot[key].(string); !ok || pystr.Strip(text) == "" {
			return false
		}
	}
	return true
}

// Reports yes when the main model or any self routed auxiliary task runs on a local server
func LocalModelProviderUsed(ctx context.Context, cfg map[string]any) (string, error) {
	var slots []any
	if model, ok := asDict(cfg["model"]); ok {
		slots = append(slots, model)
	}
	if auxiliary, ok := asDict(cfg["auxiliary"]); ok {
		for _, key := range pyDictKeys(cfg["auxiliary"], auxiliary) {
			if slot, ok := asDict(auxiliary[key]); ok && auxSlotRoutesItself(slot) {
				slots = append(slots, slot)
			}
		}
	}
	for _, slot := range slots {
		local, err := slotIsLocal(ctx, slot)
		if err != nil {
			return "", err
		}
		if local {
			return "yes", nil
		}
	}
	return "no", nil
}

// Readers the v4 snapshot calls, replaceable so tests can break each one
var installV4Readers = struct {
	behind, gpu, ram, version func(ctx context.Context) (string, error)
	local, channel            func(ctx context.Context, cfg map[string]any) (string, error)
}{
	behind:  func(ctx context.Context) (string, error) { return BehindBucket(ctx, "", 0) },
	gpu:     func(context.Context) (string, error) { return GpuClass() },
	ram:     func(context.Context) (string, error) { return RamBucket() },
	version: func(context.Context) (string, error) { return InstallVersionAgeBucket(0) },
	local:   LocalModelProviderUsed,
	channel: func(_ context.Context, cfg map[string]any) (string, error) { return ReleaseChannel(cfg) },
}

func safeInstallField(name, fallback string, reader func() (string, error)) (value string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics install field "+name+" unavailable", "error", fmt.Sprint(r))
			value = fallback
		}
	}()
	value, err := reader()
	if err != nil {
		slog.Debug("Shared-metrics install field "+name+" unavailable", "error", err)
		return fallback
	}
	return value
}

// Version lag, channel and hardware dimensions of the daily snapshot, off vocabulary values reading unknown
func InstallV4SnapshotFields(ctx context.Context, cfg map[string]any) map[string]string {
	r := installV4Readers
	fields := map[string]string{
		"behind_bucket":             safeInstallField("behind_bucket", "unknown", func() (string, error) { return r.behind(ctx) }),
		"gpu_class":                 safeInstallField("gpu_class", "unknown", func() (string, error) { return r.gpu(ctx) }),
		"local_model_provider_used": safeInstallField("<lambda>", localProviderUnknown, func() (string, error) { return r.local(ctx, cfg) }),
		"ram_bucket":                safeInstallField("ram_bucket", "unknown", func() (string, error) { return r.ram(ctx) }),
		"release_channel":           safeInstallField("<lambda>", "unknown", func() (string, error) { return r.channel(ctx, cfg) }),
		"version_age_bucket":        safeInstallField("version_age_bucket", "unknown", func() (string, error) { return r.version(ctx) }),
	}
	for key, value := range fields {
		if !installV4SnapshotDimensions[key].Has(value) {
			fields[key] = "unknown"
			if key == "local_model_provider_used" {
				fields[key] = localProviderUnknown
			}
		}
	}
	return fields
}
