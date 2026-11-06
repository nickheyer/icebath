package selfupdate

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const codeRootMaxDepth = 8

var (
	fleetStdout io.Writer = os.Stdout

	fleetCodeIdentity    = updaterecord.GetCodeIdentity
	fleetSocketIdentity  = socketIdentity
	fleetHomes           = fleetProfileHomes
	fleetReadStatus      = gateway.ReadRuntimeStatus
	fleetStatusPidIsLive = gateway.RuntimeStatusPidIsLive
	fleetLiveGatewayPID  = gateway.LiveGatewayPidForHome

	notExpectedStates = map[string]bool{"stopped": true, "startup_failed": true}

	fleetRowLines = map[string]string{
		"current":                        "  ✓ %[1]s (pid %[2]s) @ %[3]s — up to date",
		"stale":                          "  ✗ %[1]s (pid %[2]s) @ %[3]s — STALE (pre-update code)",
		"down":                           "  ✗ %[1]s — DOWN (gateway was running before the update; pid %[2]s is gone and nothing replaced it)",
		updaterecord.ExternalState:       "  ◆ %[1]s (pid %[2]s) @ %[3]s — separate checkout, not updated by this run",
		updaterecord.RestartPendingState: "  ↻ %[1]s (pid %[2]s) @ %[3]s — restart pending (deferred until this process exits; the update runs inside this gateway)",
	}
	fleetRowUnknown         = "  ? %[1]s (pid %[2]s) — version unknown (gateway predates version stamping; restart to enable)"
	fleetRowIdentityPending = "  ? %[1]s (pid %[2]s) — new pid since the update, code identity not published yet — re-check with `icebath gateway status`"
)

type profileHome struct{ profile, home string }

// Default home plus every valid named profile dir, sorted by name
func fleetProfileHomes() []profileHome {
	var homes []profileHome
	if st, err := os.Stat(profiles.DefaultHome()); err == nil && st.IsDir() {
		homes = append(homes, profileHome{"default", profiles.DefaultHome()})
	}
	root := profiles.ProfilesRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return homes
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		path := filepath.Join(root, e.Name())
		if st, err := os.Stat(path); err == nil && st.IsDir() && e.Name() != "default" && home.ProfileIDRe.MatchString(e.Name()) {
			homes = append(homes, profileHome{e.Name(), path})
		}
	}
	return homes
}

// Pid and identity the gateway owning a home's control socket declares, false when none answers
func socketIdentity(h string) (int, *pyjson.Object, bool) {
	identity := gateway.IdentifyGateway(h, gateway.DefaultControlClientTimeout)
	if identity == nil || identity.Len() == 0 {
		return 0, nil, false
	}
	raw, _ := identity.Get("pid")
	switch t := raw.(type) {
	case float64:
		if t != t || t > 1<<62 || t < -(1<<62) {
			return 0, nil, false
		}
		return int(t), identity, true
	case string:
		n, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(t), "_", ""))
		if err != nil {
			return 0, nil, false
		}
		return n, identity, true
	}
	pid, ok := pyrepr.Int(raw)
	if !ok || !pid.IsInt64() {
		return 0, nil, false
	}
	return int(pid.Int64()), identity, true
}

// Install root holding the icebath binary that contains an absolute process path
func codeRootForPath(raw any) string {
	text, ok := raw.(string)
	if !ok || text == "" || !filepath.IsAbs(text) {
		return ""
	}
	binary := "icebath"
	if runtime.GOOS == "windows" {
		binary = "icebath.exe"
	}
	candidate := filepath.Clean(text)
	for depth := 0; depth < codeRootMaxDepth; depth++ {
		if st, err := os.Stat(filepath.Join(candidate, binary)); err == nil && st.Mode().IsRegular() {
			resolved, err := pypath.Resolve(candidate)
			if err != nil {
				return ""
			}
			return resolved
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			break
		}
		candidate = parent
	}
	return ""
}

// Install root of the running updater binary
func updaterCodeRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := pypath.Resolve(exe); err == nil {
		exe = resolved
	}
	return codeRootForPath(exe)
}

// Install a verified live gateway serves, from its status argv then its process exe and cmdline
func gatewayCodeRoot(pid int, h string) string {
	if record := fleetReadStatus(filepath.Join(h, "gateway_state.json")); record != nil {
		raw, _ := record.Get("pid")
		if n, ok := pyrepr.Int(raw); ok && n.IsInt64() && int(n.Int64()) == pid {
			argv, _ := record.Get("argv")
			if items, ok := argv.([]any); ok {
				for _, probe := range items {
					if root := codeRootForPath(probe); root != "" {
						return root
					}
				}
			}
		}
	}
	var probes []any
	if exe, err := proc.Exe(pid); err == nil {
		probes = append(probes, exe)
	}
	if cmdline, err := proc.Cmdline(pid); err == nil {
		for _, a := range cmdline {
			probes = append(probes, a)
		}
	}
	for _, probe := range probes {
		if root := codeRootForPath(probe); root != "" {
			return root
		}
	}
	return ""
}

// One fleet matrix row classified against the expected identity and install
func fleetRow(profile string, pid int, codeSHA, codeVersion, expectedSHA any, state, codeRoot, expectedRoot string,
	servedProfiles any, selfRestartPending map[int]bool) *pyjson.Object {
	if state == "unknown" && codeRoot != "" && expectedRoot != "" && codeRoot != expectedRoot {
		state = updaterecord.ExternalState
	}
	if state == "unknown" && truthyValue(codeSHA) && truthyValue(expectedSHA) {
		state = "stale"
		if pyrepr.Str(codeSHA) == pyrepr.Str(expectedSHA) {
			state = "current"
		}
	}
	if state == "stale" && selfRestartPending[pid] {
		state = updaterecord.RestartPendingState
	}
	row := pyjson.ObjectOf("profile", profile, "pid", pid, "code_sha", nil, "code_version", codeVersion, "state", state, "code_root", nil)
	if truthyValue(codeSHA) {
		row.Set("code_sha", pyrepr.Str(codeSHA))
	}
	if codeRoot != "" {
		row.Set("code_root", codeRoot)
	}
	if names, ok := servedProfiles.([]any); ok && len(names) > 0 {
		var unique []any
		seen := map[string]bool{}
		valid := true
		for _, name := range names {
			s, ok := name.(string)
			if !ok || s == "" {
				valid = false
				break
			}
			if !seen[s] {
				seen[s] = true
				unique = append(unique, s)
			}
		}
		if valid {
			row.Set("served_profiles", unique)
		}
	}
	return row
}

func truthyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case bool:
		return t
	}
	return true
}

func objectGet(o *pyjson.Object, key string) any {
	if o == nil {
		return nil
	}
	v, _ := o.Get(key)
	return v
}

// Every profile gateway's code identity against the running install, down rows only for pre restart pids
func CollectFleetVersions(preRestartPIDs []int, selfRestartPending []int) []*pyjson.Object {
	preRestart, pending := map[int]bool{}, map[int]bool{}
	for _, p := range preRestartPIDs {
		preRestart[p] = true
	}
	for _, p := range selfRestartPending {
		pending[p] = true
	}
	results := []*pyjson.Object{}
	var expectedSHA any
	if identity, err := fleetCodeIdentity(true); err == nil {
		expectedSHA = objectGet(identity, "sha")
	}
	expectedRoot := updaterCodeRoot()
	for _, ph := range fleetHomes() {
		if pid, identity, ok := fleetSocketIdentity(ph.home); ok {
			row := fleetRow(ph.profile, pid, objectGet(identity, "code_sha"), objectGet(identity, "code_version"), expectedSHA,
				"unknown", gatewayCodeRoot(pid, ph.home), expectedRoot, pyjson.Plain(objectGet(identity, "served_profiles")), pending)
			row.Set("source", "socket")
			results = append(results, row)
			continue
		}
		record := fleetReadStatus(filepath.Join(ph.home, "gateway_state.json"))
		if record == nil || len(record.Vals) == 0 {
			continue
		}
		n, ok := pyrepr.Int(objectGet(record, "pid"))
		if !ok || !n.IsInt64() {
			continue
		}
		pid := int(n.Int64())
		live, found, err := fleetLiveGatewayPID(ph.home)
		if err != nil {
			slog.Debug(fmt.Sprintf("Fleet version probe failed: %s", pypath.OSErrorText(err)))
			return results
		}
		if found && live == pid {
			results = append(results, fleetRow(ph.profile, pid, objectGet(record, "code_sha"), objectGet(record, "code_version"), expectedSHA,
				"unknown", gatewayCodeRoot(pid, ph.home), expectedRoot, pyjson.Plain(objectGet(record, "served_profiles")), pending))
			continue
		}
		if fleetStatusPidIsLive(record) {
			results = append(results, fleetRow(ph.profile, pid, nil, nil, nil, "unknown", "", "", nil, nil))
			continue
		}
		if state, ok := objectGet(record, "gateway_state").(string); preRestart[pid] && ok && state != "" && !notExpectedStates[state] {
			results = append(results, fleetRow(ph.profile, pid, nil, objectGet(record, "code_version"), nil, "down", "", "", nil, nil))
		}
	}
	return results
}

// Prints the post update fleet matrix, true when a gateway is provably stale or down
func PrintFleetVersionMatrix(fleet []*pyjson.Object) bool {
	if len(fleet) == 0 {
		return false
	}
	out := fleetStdout
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Fleet version check:")
	states := map[any]bool{}
	var externalRoots []string
	staleOrDown := 0
	for _, entry := range fleet {
		states[entry.Vals["state"]] = true
		if updaterecord.RowIsExternal(entry) && truthyValue(entry.Vals["code_root"]) {
			externalRoots = append(externalRoots, fmt.Sprintf("%s: %s", pyrepr.Str(entry.Vals["profile"]), pyrepr.Str(entry.Vals["code_root"])))
		}
		line := fleetRowUnknown
		if truthyValue(entry.Vals["identity_pending"]) {
			line = fleetRowIdentityPending
		}
		if state, ok := entry.Vals["state"].(string); ok {
			if known, ok := fleetRowLines[state]; ok {
				line = known
			}
		}
		short := "?"
		if sha, ok := entry.Vals["code_sha"].(string); ok && sha != "" {
			short = sha[:min(8, len(sha))]
		}
		fmt.Fprintln(out, fmt.Sprintf(line, pyrepr.Str(entry.Vals["profile"]), pyrepr.Str(entry.Vals["pid"]), short))
		if entry.Vals["state"] == "stale" || entry.Vals["state"] == "down" {
			staleOrDown++
		}
	}
	if len(externalRoots) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  ℹ These profiles run their own checkout and are updated separately:")
		for _, line := range externalRoots {
			fmt.Fprintf(out, "      %s\n", line)
		}
	}
	if states[updaterecord.RestartPendingState] {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  ℹ A restart-pending gateway has not yet been verified on the new code;")
		fmt.Fprintln(out, "    check after this update exits with `icebath gateway status`.")
	}
	if staleOrDown > 0 {
		fmt.Fprintln(out)
		if states["stale"] {
			fmt.Fprintln(out, "  ⚠ Stale gateways keep serving pre-update code until restarted.")
		}
		if states["down"] {
			fmt.Fprintln(out, "  ⚠ Down gateways stopped serving messaging entirely.")
		}
		fmt.Fprintln(out)
		fmt.Fprintf(out, "⚠ Gateway restart still owed: %d gateway(s) still running the old code (or stopped).\n", staleOrDown)
		fmt.Fprintln(out, "  Run `icebath gateway restart` (or `icebath -p <profile> gateway restart` for a named")
		fmt.Fprintln(out, "  profile), then `icebath gateway status` to confirm.")
	}
	return staleOrDown > 0
}
