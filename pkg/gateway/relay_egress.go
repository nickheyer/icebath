package gateway

import (
	"context"
	"errors"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	EgressDeclineCode   = "egress_declined"
	EgressDeclineMarker = "egress declined:"
	ambiguousAckMarker  = "ack lost"
)

func egressLog() *slog.Logger { return slog.Default().With("logger", "gateway.relay.egress") }

// True when a connector result dict is a definite refusal carrying the decline code or uniform text
func IsEgressDecline(result any) bool {
	get, ok := dictGetter(result)
	if !ok {
		return false
	}
	if pyrepr.Truthy(get("success")) || pyrepr.Truthy(get("ambiguous")) {
		return false
	}
	if pyrepr.Str(orEmpty(get("code"))) == EgressDeclineCode {
		return true
	}
	return strings.Contains(pystr.Lower(pyrepr.Str(orEmpty(get("error")))), EgressDeclineMarker)
}

// Key lookup of a Python dict value held as a Go map or a decoded JSON object
func dictGetter(v any) (func(string) any, bool) {
	switch d := v.(type) {
	case map[string]any:
		return func(k string) any { return d[k] }, true
	case *pyjson.Object:
		if d == nil {
			return nil, false
		}
		return func(k string) any { x, _ := d.Get(k); return x }, true
	}
	return nil, false
}

// Python value or empty string, the value or "" idiom
func orEmpty(v any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return ""
}

// True when a send result carries a destination refusal in its raw response or its error text
func DeclinedSend(result *SendResult) bool {
	if result == nil {
		return false
	}
	if _, ok := dictGetter(result.RawResponse); ok {
		return IsEgressDecline(result.RawResponse)
	}
	if !pyrepr.Truthy(result.Error) {
		return false
	}
	if strings.Contains(pystr.Lower(pyrepr.Str(result.Error)), ambiguousAckMarker) {
		return false
	}
	return IsEgressDecline(map[string]any{"success": false, "error": result.Error})
}

// Connector decline text verbatim, or the generic relay egress declined text
func DeclineError(result any) string {
	if get, ok := dictGetter(result); ok {
		if e := get("error"); pyrepr.Truthy(e) {
			return pyrepr.Str(e)
		}
	}
	return "relay egress declined"
}

// Warns about a decline on a lane whose contract cannot carry an error
func LogDecline(op string, chatID, result any) {
	egressLog().Warn(fmt.Sprintf("relay %s DECLINED for %s: %s", op, pyrepr.Str(chatID), DeclineError(result)))
}

// Relay routing could not be determined, callers fail closed
type RelayRouteUnknown struct{ Msg string }

func (e *RelayRouteUnknown) Error() string { return e.Msg }

// Relay adapter answering whether it fronts a logical platform
type relayFronter interface {
	FrontsPlatform(p Platform) bool
}

// Connected relay adapter fronted set, ok false only when no runner or relay adapter exists
func liveRelayFronted() (fronted map[string]bool, ok bool, err error) {
	runner := CurrentGatewayRunner()
	if runner == nil {
		return nil, false, nil
	}
	reg, isReg := runner.(primaryAdaptersRunner)
	if !isReg {
		return nil, false, nil
	}
	defer func() {
		if r := recover(); r != nil {
			egressLog().Error("could not determine the live relay adapter's fronted set", "error", fmt.Sprint(r))
			fronted, ok, err = nil, false, &RelayRouteUnknown{"the connected relay adapter could not report which platforms it fronts, so this destination's routing could not be determined"}
		}
	}()
	registry := reg.Adapters()
	if registry == nil || registry.Len() == 0 {
		return nil, false, nil
	}
	relay, _ := registry.Get(PlatformRelay)
	if relay == nil {
		return nil, false, nil
	}
	fronts, isFronter := relay.(relayFronter)
	if !isFronter {
		panic(fmt.Sprintf("'%T' object has no attribute 'fronts_platform'", relay))
	}
	out := map[string]bool{}
	for _, p := range PlatformMembers() {
		if pystr.Lower(string(p)) != "relay" && fronts.FrontsPlatform(p) {
			out[pystr.Lower(pystr.Strip(string(p)))] = true
		}
	}
	return out, true, nil
}

// Platforms the connector fronts, the live adapter set first, RelayRouteUnknown when discovery faults
func relayFronted(ctx context.Context) (map[string]bool, error) {
	live, ok, err := liveRelayFronted()
	if err != nil {
		return nil, err
	}
	if ok {
		return live, nil
	}
	found, err := relayFrontedPlatformsGuarded(ctx)
	if err != nil {
		return nil, &RelayRouteUnknown{fmt.Sprintf("relay route discovery failed: %v", err)}
	}
	out := map[string]bool{}
	for p := range found {
		out[pystr.Lower(pystr.Strip(p))] = true
	}
	return out, nil
}

// Fronted platforms with a raised discovery fault returned as an error
func relayFrontedPlatformsGuarded(ctx context.Context) (out map[string]bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	return RelayFrontedPlatformsChecked(ctx)
}

// Whether this process runs an enabled native adapter for the platform
func hasLiveNativeAdapter(ctx context.Context, platformName string) (live bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			live, err = false, nil
		}
	}()
	runner := CurrentGatewayRunner()
	if runner == nil {
		return false, nil
	}
	var adapters *pydict.Dict[Platform, PlatformAdapter]
	if reg, ok := runner.(primaryAdaptersRunner); ok {
		adapters = reg.Adapters()
	}
	platform, perr := ParsePlatform(ctx, platformName)
	if perr != nil {
		return false, nil
	}
	var native PlatformAdapter
	if adapters != nil {
		native, _ = adapters.Get(platform)
	}
	if native == nil {
		return false, nil
	}
	cfg, cerr := LoadGatewayConfig(ctx)
	if cerr != nil {
		return false, &RelayRouteUnknown{fmt.Sprintf("native-adapter config lookup failed for %s: %v", platformName, cerr)}
	}
	platformCfg := cfg.Platforms[platform]
	return platformCfg == nil || platformCfg.Enabled, nil
}

// Whether a send to the platform would egress over the relay connector
func RelayRoutedPlatform(ctx context.Context, platformName string) (bool, error) {
	name := pystr.Lower(pystr.Strip(platformName))
	if name == "" {
		return false, nil
	}
	if name == "relay" {
		return true, nil
	}
	fronted, err := relayFronted(ctx)
	if err != nil {
		return false, err
	}
	if !fronted[name] {
		return false, nil
	}
	live, err := hasLiveNativeAdapter(ctx, name)
	if err != nil {
		return false, err
	}
	return !live, nil
}

// Configured home channel id of the platform, empty when unset or unreadable
func egressHomeChannelID(ctx context.Context, platformName string) string {
	cfg, err := LoadGatewayConfig(ctx)
	if err != nil {
		return ""
	}
	platform, err := ParsePlatform(ctx, platformName)
	if err != nil {
		return ""
	}
	if home := cfg.GetHomeChannel(platform); home != nil && home.ChatID != "" {
		return home.ChatID
	}
	return ""
}

// Directory ids of the platform, an error where iterating a non iterable entry list raises
func egressDirectoryIDs(ctx context.Context, platformName string) (map[string]bool, error) {
	ids := map[string]bool{}
	platforms, _ := loadChannelDirectory(ctx).Get("platforms")
	if platforms == nil {
		platforms = pyjson.NewObject()
	}
	pobj, ok := platforms.(*pyjson.Object)
	if !ok {
		return ids, nil
	}
	entries, _ := pobj.Get(platformName)
	if !pyrepr.Truthy(entries) {
		return ids, nil
	}
	switch list := entries.(type) {
	case []any:
		for _, e := range list {
			if entry, ok := e.(*pyjson.Object); ok {
				if id, _ := entry.Get("id"); pyrepr.Truthy(id) {
					ids[pyrepr.Str(id)] = true
				}
			}
		}
	case string, *pyjson.Object:
	default:
		return ids, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(entries))
	}
	return ids, nil
}

// Chat ids this gateway held a session in, with thread suffixed ids also listed bare
func egressSessionIDs(ctx context.Context, platformName string) map[string]bool {
	ids := map[string]bool{}
	for _, entry := range BuildFromSessions(ctx, platformName) {
		id, _ := entry.Get("id")
		if !pyrepr.Truthy(id) {
			continue
		}
		raw := pyrepr.Str(id)
		ids[raw] = true
		if tid, _ := entry.Get("thread_id"); pyrepr.Truthy(tid) {
			if suffix := ":" + pyrepr.Str(tid); strings.HasSuffix(raw, suffix) {
				ids[raw[:len(raw)-len(suffix)]] = true
			}
		}
	}
	return ids
}

// Chat ids with a home channel, directory or session provenance, every fronted platform for relay
func AttestedRelayTargets(ctx context.Context, platformName string) (map[string]bool, error) {
	name := pystr.Lower(pystr.Strip(platformName))
	names := []string{name}
	if name == "relay" {
		fronted, err := relayFronted(ctx)
		if err != nil {
			return nil, err
		}
		for _, p := range sortedSet(fronted) {
			if p != name {
				names = append(names, p)
			}
		}
	}
	attested := map[string]bool{}
	for _, candidate := range names {
		if home := egressHomeChannelID(ctx, candidate); home != "" {
			attested[home] = true
		}
		dir, err := egressDirectoryIDs(ctx, candidate)
		if err != nil {
			return nil, err
		}
		for id := range dir {
			attested[id] = true
		}
		for id := range egressSessionIDs(ctx, candidate) {
			attested[id] = true
		}
	}
	return attested, nil
}

// Whether the gateway holds an enabled token for the platform, true on any probe fault
func hasNativeCredential(ctx context.Context, platformName string) (has bool) {
	defer func() {
		if r := recover(); r != nil {
			egressLog().Debug("native-credential probe failed; withdrawing the exemption", "error", fmt.Sprint(r))
			has = true
		}
	}()
	cfg, err := LoadGatewayConfig(ctx)
	if err != nil {
		egressLog().Debug("native-credential probe failed; withdrawing the exemption", "error", err.Error())
		return true
	}
	platform, err := ParsePlatform(ctx, platformName)
	if err != nil {
		egressLog().Debug("native-credential probe failed; withdrawing the exemption", "error", err.Error())
		return true
	}
	pc := cfg.Platforms[platform]
	return pc != nil && pc.Enabled && pyrepr.Truthy(pc.Token)
}

// Snapshot of the native token a send will use, Set false when the caller passed none
type NativeToken struct {
	Set   bool
	Token any
}

// Whether the target is a Telegram at handle the gateway cannot compare against resolved ids
func isUnresolvedHandle(ctx context.Context, platformName, target string, native NativeToken) bool {
	if platformName != "telegram" || !strings.HasPrefix(target, "@") {
		return false
	}
	if native.Set {
		return !pyrepr.Truthy(native.Token)
	}
	return !hasNativeCredential(ctx, platformName)
}

// Refusal text when a relay destination is unattested, empty when the send may proceed
func AuthorizeRelayTarget(ctx context.Context, platformName string, chatID, threadID any, native NativeToken) (string, error) {
	routed, err := RelayRoutedPlatform(ctx, platformName)
	var unknown *RelayRouteUnknown
	if errors.As(err, &unknown) {
		egressLog().Warn(fmt.Sprintf("relay routing unknown for %s — refusing the send: %s", platformName, unknown.Msg))
		return fmt.Sprintf("Refusing to send to '%s': relay routing could not be determined, so this destination could not be verified.", platformName), nil
	}
	if err != nil {
		return "", err
	}
	if !routed {
		return "", nil
	}
	target := pystr.Strip(pyrepr.Str(orEmpty(chatID)))
	if target == "" {
		return "", nil
	}
	name := pystr.Lower(pystr.Strip(platformName))
	attested, err := AttestedRelayTargets(ctx, name)
	if err != nil {
		return "", err
	}
	if attested[target] {
		thread := pystr.Strip(pyrepr.Str(orEmpty(threadID)))
		if thread != "" && thread != target {
			if attested[target+":"+thread] {
				return "", nil
			}
			return fmt.Sprintf("Refusing to send to relay target '%s:%s:%s': the parent chat is attested but this gateway has no record of the thread, and on this platform the thread IS the delivery destination.", name, target, thread), nil
		}
		return "", nil
	}
	if isUnresolvedHandle(ctx, name, target, native) {
		egressLog().Debug(fmt.Sprintf("relay target '%s:%s' is an unresolved handle — deferring authorization to the connector's egress floor", name, target))
		return "", nil
	}
	return fmt.Sprintf("Refusing to send to unattested relay target '%s:%s': this gateway has no record of that destination. Use send_message(action='list') to see the targets it can reach.", name, target), nil
}
