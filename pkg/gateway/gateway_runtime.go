package gateway

import (
	"context"
	"fmt"
	"math/big"
	"net/netip"
	"path/filepath"
	"runtime"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// DefaultEnsureTimeout is the total startup deadline a client waits for a cold daemon
const DefaultEnsureTimeout = 60 * time.Second

// GatewayEndpoint is the validated identity of a live gateway owner
type GatewayEndpoint struct {
	ProfileID       string
	InstanceID      string
	AuthorityEpoch  *big.Int
	RuntimeProtocol int
	APIOrigin       string
	Supervisor      string
	Capabilities    map[string]bool
	ControlHome     string
	CodeSHA         string
}

// ObservedEndpoint is a discovery or ensure verdict, empty strings standing for None
type ObservedEndpoint struct {
	State      string
	ReasonCode string
	Endpoint   *GatewayEndpoint
	Detail     string
}

// Whether a client may attach to a verdict, ready owners plus a restart's after turn wait
func Attachable(result ObservedEndpoint) bool {
	if result.Endpoint == nil {
		return false
	}
	return result.State == "ready" || (result.State == "draining" && result.ReasonCode == RestartWaitReason)
}

// Profile home whose gateway serves home, the owner a private overlay's lock names
func GatewayProfileHome(home string) (string, error) {
	resolved, err := pypath.Resolve(pypath.Expanduser(home))
	if err != nil {
		return "", err
	}
	return overlayOwnerHome(resolved), nil
}

var validSupervisors = map[string]bool{"none": true, "systemd": true, "launchd": true, "windows": true, "external": true}

func unhashable(v any) bool {
	switch v.(type) {
	case []any, *pyjson.Object, map[string]any:
		return true
	}
	return false
}

// Verdict of an identify payload for home, errors where the Python comparison raises
func endpointFromPayload(payload *pyjson.Object, home, controlHome string) (ObservedEndpoint, error) {
	if rp := recGet(payload, "runtime_protocol"); !pyIsInt(rp) || !pyEq(rp, 1) {
		return ObservedEndpoint{State: "incompatible", ReasonCode: "runtime_protocol"}, nil
	}
	profilesList, ok := recGet(payload, "served_profiles").([]any)
	if !ok {
		return ObservedEndpoint{State: "inaccessible", ReasonCode: "profile_mismatch"}, nil
	}
	wanted, err := CanonicalHome(home)
	if err != nil {
		return ObservedEndpoint{}, err
	}
	var matches []*pyjson.Object
	for _, p := range profilesList {
		entry, ok := p.(*pyjson.Object)
		if !ok {
			continue
		}
		h, ok := recGet(entry, "home").(string)
		if !ok {
			continue
		}
		canon, err := CanonicalHome(h)
		if err != nil {
			return ObservedEndpoint{}, err
		}
		if canon == wanted {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		return ObservedEndpoint{State: "inaccessible", ReasonCode: "profile_mismatch"}, nil
	}
	profileID, ok := recGet(matches[0], "profile_id").(string)
	if !ok || profileID == "" {
		return ObservedEndpoint{State: "inaccessible", ReasonCode: "profile_mismatch"}, nil
	}
	state := recGet(payload, "state")
	drainReason, _ := recGet(payload, "drain_reason").(string)
	restartWait := state == "draining" && drainReason == RestartWaitReason
	if unhashable(state) {
		return ObservedEndpoint{}, fmt.Errorf("unhashable type: '%s'", pyTypeName(state))
	}
	if s, _ := state.(string); (s == "starting" || s == "draining" || s == "conflict") && !restartWait {
		reason := ""
		if s == "draining" && drainReason == "idle_exit" {
			reason = "idle_exit"
		}
		return ObservedEndpoint{State: s, ReasonCode: reason}, nil
	}
	if state != "ready" && !restartWait {
		return ObservedEndpoint{State: "inaccessible", ReasonCode: "invalid_runtime_state"}, nil
	}
	capList, ok := recGet(payload, "capabilities").([]any)
	capabilities := map[string]bool{}
	for _, c := range capList {
		s, isStr := c.(string)
		if !isStr {
			ok = false
			break
		}
		capabilities[s] = true
	}
	if !ok || !capabilities["session-authority-v1"] {
		return ObservedEndpoint{State: "incompatible", ReasonCode: "session_authority_unavailable"}, nil
	}
	epoch := recGet(payload, "authority_epoch")
	instance, instanceOK := recGet(payload, "instance_id").(string)
	epochInt, epochOK := pyrepr.Int(epoch)
	if !pyIsInt(epoch) || !epochOK || epochInt.Sign() <= 0 || !instanceOK || instance == "" {
		return ObservedEndpoint{State: "inaccessible", ReasonCode: "invalid_runtime_identity"}, nil
	}
	origin, ok := recGet(payload, "api_origin").(string)
	if !ok || !loopbackOrigin(origin) {
		return ObservedEndpoint{State: "inaccessible", ReasonCode: "invalid_api_origin"}, nil
	}
	supervisor := recGet(payload, "supervisor")
	if unhashable(supervisor) {
		return ObservedEndpoint{}, fmt.Errorf("unhashable type: '%s'", pyTypeName(supervisor))
	}
	sup, _ := supervisor.(string)
	if !validSupervisors[sup] {
		return ObservedEndpoint{State: "inaccessible", ReasonCode: "invalid_supervisor"}, nil
	}
	codeSHA, _ := recGet(payload, "code_sha").(string)
	endpoint := &GatewayEndpoint{
		ProfileID: profileID, InstanceID: instance, AuthorityEpoch: epochInt, RuntimeProtocol: 1,
		APIOrigin: origin, Supervisor: sup, Capabilities: capabilities, ControlHome: controlHome, CodeSHA: codeSHA,
	}
	if restartWait {
		return ObservedEndpoint{State: "draining", Endpoint: endpoint, ReasonCode: RestartWaitReason}, nil
	}
	return ObservedEndpoint{State: "ready", Endpoint: endpoint}, nil
}

func pyTypeName(v any) string {
	switch v.(type) {
	case []any:
		return "list"
	}
	return "dict"
}

// Whether origin is a bare http or https loopback origin with a port and no credentials
func loopbackOrigin(origin string) bool {
	parsed, err := pyurl.Split(origin)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || host == "" {
		return false
	}
	for i := 0; i < len(parsed.Netloc); i++ {
		if parsed.Netloc[i] == '@' {
			return false
		}
	}
	if parsed.Path != "" || parsed.Query != "" || parsed.Fragment != "" {
		return false
	}
	port, has, err := parsed.Port()
	if err != nil || !has || port == 0 {
		return false
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// Home whose control socket mints tickets for endpoint, the multiplexer's for a served profile
func ControlHomeFor(home string, endpoint *GatewayEndpoint) string {
	if endpoint != nil && endpoint.ControlHome != "" {
		return endpoint.ControlHome
	}
	return home
}

// Terminal verdict for a secondary the multiplexer published under parked_profiles
func parkedByMultiplexer(payload *pyjson.Object, home string) (*ObservedEndpoint, error) {
	parked, ok := recGet(payload, "parked_profiles").(*pyjson.Object)
	if !ok {
		return nil, nil
	}
	name, err := profiles.NormalizeProfileName(filepath.Base(home))
	if err != nil {
		return nil, err
	}
	for _, key := range parked.Keys {
		candidate, err := profiles.NormalizeProfileName(key)
		if err != nil {
			return nil, err
		}
		if candidate != name {
			continue
		}
		reason, _ := parked.Get(key)
		if reason == nil {
			return nil, nil
		}
		detail := []rune(pyrepr.Str(reason))
		if len(detail) > 500 {
			detail = detail[:500]
		}
		return &ObservedEndpoint{State: "inaccessible", ReasonCode: "profile_parked", Detail: string(detail)}, nil
	}
	return nil, nil
}

// Named refusal when the client proved the boundary from the control pipe
func refusedOwnerVerdict(err error) *ObservedEndpoint {
	if refused, ok := err.(*RefusedOwner); ok {
		return &ObservedEndpoint{State: "inaccessible", ReasonCode: refused.ReasonCode, Detail: refused.Error()}
	}
	return nil
}

// Verdict of the default multiplexer serving home, nil when it does not prove service
func servedByMultiplexer(ctx context.Context, home string, timeout time.Duration) (*ObservedEndpoint, error) {
	root := MultiplexerRootFor(home)
	if root == "" {
		return nil, nil
	}
	payload, err := queryIdentifySeam(root, timeout)
	if err != nil {
		if discoveryErrorKind(err) != "permission" {
			return nil, nil
		}
		named := refusedOwnerVerdict(err)
		if named == nil {
			return nil, nil
		}
		verdict, known, err := HostServesNamedProfile(ctx, root, home)
		if err != nil {
			return nil, err
		}
		if known && !verdict {
			return nil, nil
		}
		return named, nil
	}
	result, err := endpointFromPayload(payload, home, root)
	if err != nil {
		return nil, err
	}
	if result.State == "inaccessible" && result.ReasonCode == "profile_mismatch" {
		return parkedByMultiplexer(payload, home)
	}
	return &result, nil
}

// Swappable host fact of refusal naming and the refusal verdict discovery reports
var (
	nativeWindows = runtime.GOOS == "windows"
	refusedSeam   = func(home string) ObservedEndpoint { return refusedVerdict(home, nativeWindows) }
)

// Pid a refusal may name, one of home's gateway records provably that live gateway
func ownerPid(home string) (int, bool) {
	for _, name := range []string{"gateway.pid", "gateway.lock"} {
		record := readJSONFile(filepath.Join(home, name), true)
		pid, ok := liveFromRecord(record)
		if !ok || pid == statusGetpid() {
			continue
		}
		recorded := recGet(record, "start_time")
		if recorded == nil || !pyEq(recorded, statusProcessStartTime(pid)) {
			continue
		}
		if RecordedGatewayHomeConflicts(record, home) {
			continue
		}
		if !recordMatchesLiveGatewayPid(record, pid, "") {
			continue
		}
		return pid, true
	}
	return 0, false
}

// Refusal verdict naming a Windows elevation or session boundary, authorization otherwise
func refusedVerdict(home string, windows bool) ObservedEndpoint {
	if windows {
		if pid, ok := ownerPid(home); ok {
			if reason, remedy, found, err := probeRefusedOwnerSeam(pid); err == nil && found {
				return ObservedEndpoint{State: "inaccessible", ReasonCode: reason, Detail: remedy}
			}
		}
	}
	return ObservedEndpoint{State: "inaccessible", ReasonCode: "authorization"}
}

// Live control identity of a profile home, preserving uncertainty instead of spawning
func DiscoverGatewayEndpoint(ctx context.Context, home string, timeout time.Duration) ObservedEndpoint {
	observed, err := discoverGatewayEndpoint(ctx, home, timeout)
	if err != nil {
		panic(err)
	}
	return observed
}

func discoverGatewayEndpoint(ctx context.Context, profileHome string, timeout time.Duration) (ObservedEndpoint, error) {
	if timeout <= 0 {
		return ObservedEndpoint{}, fmt.Errorf("timeout must be finite and positive")
	}
	canonical, err := CanonicalHome(profileHome)
	if err != nil {
		return ObservedEndpoint{}, err
	}
	home := overlayOwnerHome(canonical)
	payload, err := queryIdentifySeam(home, timeout)
	if err == nil {
		observed, err := endpointFromPayload(payload, home, "")
		if err != nil {
			return ObservedEndpoint{State: "inaccessible", ReasonCode: "invalid_control_peer"}, nil
		}
		return observed, nil
	}
	switch discoveryErrorKind(err) {
	case "missing":
		served, err := servedByMultiplexer(ctx, home, timeout)
		if err != nil || served != nil {
			if served == nil {
				return ObservedEndpoint{}, err
			}
			return *served, nil
		}
		state := MissingOwnerState(home)
		if state == "inaccessible" && runtime.GOOS != "windows" {
			if detail, ok := UnreadableLockRecovery(filepath.Join(home, "gateway.lock")); ok && detail != "" {
				return ObservedEndpoint{State: state, ReasonCode: "foreign_stale_lock", Detail: detail}, nil
			}
		}
		return ObservedEndpoint{State: state}, nil
	case "timeout":
		return ObservedEndpoint{State: "inaccessible", ReasonCode: "control_timeout"}, nil
	case "discovery":
		de := err.(*DiscoveryError)
		return ObservedEndpoint{State: "inaccessible", ReasonCode: de.Reason, Detail: de.BoundedDetail(500)}, nil
	case "permission":
		if named := refusedOwnerVerdict(err); named != nil {
			return *named, nil
		}
		return refusedSeam(home), nil
	}
	return ObservedEndpoint{State: "inaccessible", ReasonCode: "invalid_control_peer"}, nil
}

// Reservation state of a home with no control socket, starting while its runtime lock is held
func MissingOwnerState(home string) string {
	lock := filepath.Join(home, "gateway.lock")
	if err := privateLock(lock); err != nil {
		if discoveryErrorKind(err) == "missing" {
			return "absent"
		}
		return "inaccessible"
	}
	active, err := statusIsGatewayRuntimeLockActiveStrict(lock)
	if err != nil {
		return "inaccessible"
	}
	if active {
		return "starting"
	}
	return "absent"
}
