package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Pid of the default profile's gateway when a verified live process owns it
func LiveDefaultGatewayPid() (int, bool, error) {
	return LiveGatewayPidForHome(home.Root())
}

// served_profiles the live default gateway recorded, recorded false when the key is absent or no gateway runs
func RecordedServedProfiles(defaultRoot string) ([]string, bool, error) {
	if _, live, err := LiveDefaultGatewayPid(); err != nil || !live {
		return nil, false, err
	}
	if defaultRoot == "" {
		defaultRoot = home.Root()
	}
	runtime := ReadRuntimeStatus(pypath.PathJoin(defaultRoot, runtimeStatusFile))
	served, ok := recGet(runtime, "served_profiles").([]any)
	if !ok {
		return nil, false, nil
	}
	out := make([]string, len(served))
	for i, p := range served {
		out[i] = pyrepr.Str(p)
	}
	return out, true, nil
}

// Named profiles the live default multiplexer serves, default excluded
func MultiplexerServedSecondaries() ([]string, error) {
	recorded, _, err := RecordedServedProfiles("")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range recorded {
		if p != "" && p != "default" {
			out = append(out, p)
		}
	}
	return out, nil
}

// Live default record of the multiplexer, nil when no verified gateway owns the default home
func liveDefaultRuntime() (*pyjson.Object, error) {
	if _, live, err := LiveDefaultGatewayPid(); err != nil || !live {
		return nil, err
	}
	runtime := ReadRuntimeStatus(pypath.PathJoin(home.Root(), runtimeStatusFile))
	if runtime == nil {
		runtime = pyjson.NewObject()
	}
	return runtime, nil
}

// Platform to reason for a served profile's platforms the multiplexer deliberately does not run
func ServedProfileUnservedPlatforms(profile string) (*pyjson.Object, error) {
	out := pyjson.NewObject()
	if profile == "" {
		return out, nil
	}
	runtime, err := liveDefaultRuntime()
	if err != nil || runtime == nil {
		return out, err
	}
	platforms, ok := recGet(runtime, "platforms").(*pyjson.Object)
	if !ok {
		return out, nil
	}
	prefix := profile + ":"
	for _, key := range platforms.Keys {
		entry, isDict := recGet(platforms, key).(*pyjson.Object)
		if !isDict || !strings.HasPrefix(key, prefix) {
			continue
		}
		code := recGet(entry, "error_code")
		if !pyEq(code, "multiplex_shared_ingress") && !pyEq(code, "whatsapp_unpaired") {
			continue
		}
		reason := recGet(entry, "error_message")
		if !pyrepr.Truthy(reason) {
			reason = "not served under multiplex"
		}
		out.Set(key[len(prefix):], pyrepr.Str(reason))
	}
	return out, nil
}

// Profile to platform to URL for every secondary inbound platform the live multiplexer serves, narrowed to profile when set
func ServedProfileIngressURLs(profile string) (*pyjson.Object, error) {
	urls := pyjson.NewObject()
	runtime, err := liveDefaultRuntime()
	if err != nil || runtime == nil {
		return urls, err
	}
	platforms, ok := recGet(runtime, "platforms").(*pyjson.Object)
	if !ok {
		return urls, nil
	}
	put := func(name, platform string, url any) {
		inner, ok := recGet(urls, name).(*pyjson.Object)
		if !ok {
			inner = pyjson.NewObject()
			urls.Set(name, inner)
		}
		inner.Set(platform, pyrepr.Str(url))
	}
	for _, key := range platforms.Keys {
		entry, isDict := recGet(platforms, key).(*pyjson.Object)
		if !isDict || !strings.Contains(key, ":") {
			continue
		}
		url := recGet(entry, "ingress_url")
		state := recGet(entry, "state")
		if !pyrepr.Truthy(url) || pyEq(state, "fatal") || pyEq(state, "disconnected") || pyEq(state, "stopped") {
			continue
		}
		name, platform, _ := strings.Cut(key, ":")
		if profile != "" && name != profile {
			continue
		}
		put(name, platform, url)
	}
	servedRaw := recGet(runtime, "served_profiles")
	var served []string
	if pyrepr.Truthy(servedRaw) {
		items, err := pyList(servedRaw)
		if err != nil {
			return nil, err
		}
		for _, p := range items {
			if pyrepr.Truthy(p) && !pyEq(p, "default") {
				served = append(served, pyrepr.Str(p))
			}
		}
	}
	for _, name := range served {
		if profile != "" && name != profile {
			continue
		}
		mirrors := SharedListenerMirrorPlatformsFor(runtime, name)
		for _, platform := range mirrors.Keys {
			entry := recGet(mirrors, platform).(*pyjson.Object)
			if url := recGet(entry, "ingress_url"); pyrepr.Truthy(url) {
				put(name, platform, url)
			}
		}
	}
	return urls, nil
}

// One indented platform and URL line per platform, sorted by platform
func FormatIngressURLLines(urls map[string]string, indent string) []string {
	platforms := make([]string, 0, len(urls))
	for p := range urls {
		platforms = append(platforms, p)
	}
	sort.Strings(platforms)
	lines := make([]string, len(platforms))
	for i, p := range platforms {
		lines[i] = indent + p + ": " + urls[p]
	}
	return lines
}

// Asks the live default multiplexer to rescan profiles, the served list it answered or false when none answered
func NotifyMultiplexerProfilesChanged(ctx context.Context, profileName string, timeout time.Duration) ([]string, bool) {
	answer, err := func() (any, error) {
		if _, live, err := LiveDefaultGatewayPid(); err != nil || !live {
			return nil, err
		}
		return rescanGatewayProfiles(ctx, home.Root(), timeout)
	}()
	if err != nil {
		slog.Debug(fmt.Sprintf("multiplexer rescan notification failed for %s: %v", pyrepr.Repr(profileName), err))
		return nil, false
	}
	obj, ok := answer.(*pyjson.Object)
	if !ok {
		if m, isMap := answer.(map[string]any); isMap {
			obj = pyjson.NewObject()
			for k, v := range m {
				obj.Set(k, v)
			}
		} else {
			return nil, false
		}
	}
	if multiplex, isBool := recGet(obj, "multiplex").(bool); isBool && !multiplex {
		return nil, false
	}
	served, ok := recGet(obj, "served_profiles").([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(served))
	for i, p := range served {
		out[i] = pyrepr.Str(p)
	}
	return out, true
}
