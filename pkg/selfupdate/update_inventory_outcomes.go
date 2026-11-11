package selfupdate

import (
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Restart bookkeeping a plan is reconciled against, a nil pid set or pid map meaning that probe did not run
type runtimeOutcomeEvidence struct {
	RestartedServices            []string
	RelaunchedProfiles           []string
	ExternallySupervisedProfiles []string
	KilledPIDs                   map[int]bool
	FailedUnits                  []string
	StaleServePIDs               map[int]bool
	FailedRespawnPIDs            map[int]bool
	ExternalGatewayPIDs          map[int]bool
	LiveGatewayPIDs              map[string]map[int]bool
}

// Bare unit or label name of a service value, its service suffix and scope prefix dropped
func bareServiceName(service any) string {
	name := strings.TrimSuffix(pyrepr.Str(service), ".service")
	return name[strings.LastIndex(name, "/")+1:]
}

// True when unit names the serve or dashboard unit of a profile exactly
func serveUnitMatchesProfile(profile string, unit any) bool {
	suffix := "-" + profile
	if profile == "default" {
		suffix = ""
	}
	name := bareServiceName(unit)
	return name == "icebath-serve"+suffix || name == "icebath-dashboard"+suffix
}

// True when service names the gateway unit, launchd label or s6 service of a profile exactly
func gatewayServiceMatchesProfile(profile string, service any) bool {
	name := bareServiceName(service)
	if profile == "default" {
		return name == "icebath-gateway" || name == "ai.icebath.gateway" || name == "gateway" || name == "gateway-default"
	}
	return name == "icebath-gateway-"+profile || name == "ai.icebath.gateway-"+profile || name == "gateway-"+profile
}

// True when any of the names is the gateway service of the runtime's profile
func gatewayNamedIn(r *RuntimeRecord, names []string) bool {
	for _, name := range names {
		if gatewayServiceMatchesProfile(r.Profile, name) {
			return true
		}
	}
	return false
}

// One row of kind, profile, pid, mechanism and outcome per planned runtime, reconciled against the restart bookkeeping
func matchRuntimeOutcomes(plan *UpdatePlan, e runtimeOutcomeEvidence) []*pyjson.Object {
	outcomes := []*pyjson.Object{}
	if plan == nil {
		return outcomes
	}
	relaunched := map[string]bool{}
	for _, profile := range e.RelaunchedProfiles {
		relaunched[profile] = true
	}
	for _, profile := range e.ExternallySupervisedProfiles {
		relaunched[profile] = true
	}
	killedHere := func(r *RuntimeRecord) bool { return r.PID != nil && e.KilledPIDs[*r.PID] }
	bookkeepingResolves := func(r *RuntimeRecord) bool {
		return killedHere(r) || relaunched[r.Profile] || gatewayNamedIn(r, e.FailedUnits) || gatewayNamedIn(r, e.RestartedServices)
	}
	pendingGateways := map[string]int{}
	for _, planned := range plan.Runtimes {
		if planned != nil && planned.Kind == "gateway" && !bookkeepingResolves(planned) {
			pendingGateways[planned.Profile]++
		}
	}
	serveUnitIn := func(r *RuntimeRecord, names []string) bool {
		for _, name := range names {
			if serveUnitMatchesProfile(r.Profile, name) {
				return true
			}
		}
		return false
	}
	outcome := func(r *RuntimeRecord) string {
		if serveKinds[r.Kind] {
			switch {
			case killedHere(r):
				return "stopped"
			case (r.PID != nil && e.FailedRespawnPIDs[*r.PID]) || serveUnitIn(r, e.FailedUnits):
				return "failed"
			case e.StaleServePIDs != nil && (r.PID == nil || !e.StaleServePIDs[*r.PID]):
				return "restarted"
			case ClientOwnedServeSupervisors[r.Supervisor]:
				if e.StaleServePIDs != nil {
					return "deferred"
				}
				return "unaccounted"
			case e.StaleServePIDs != nil:
				return "unaccounted"
			case serveUnitIn(r, e.RestartedServices):
				return "restarted"
			}
			return "unaccounted"
		}
		switch {
		case relaunched[r.Profile]:
			return "restarted"
		case killedHere(r):
			return "stopped"
		case gatewayNamedIn(r, e.FailedUnits):
			return "failed"
		case gatewayNamedIn(r, e.RestartedServices):
			return "restarted"
		}
		if e.LiveGatewayPIDs != nil && r.PID != nil {
			live := e.LiveGatewayPIDs[r.Profile]
			ambiguous := pendingGateways[r.Profile] > 1
			if len(live) > 0 && !live[*r.PID] && !ambiguous {
				return "restarted"
			}
			if ambiguous {
				inventoryLog.Debug(fmt.Sprintf("%d planned gateway runtimes for profile %s still need a verdict — successor evidence cannot attribute the restart, leaving pid %d on the name path",
					pendingGateways[r.Profile], pyrepr.Repr(r.Profile), *r.PID))
			} else if len(live) == 0 {
				inventoryLog.Debug(fmt.Sprintf("No post-restart gateway evidence for profile %s (planned pid %d via %s) — reconciling on restart bookkeeping alone",
					pyrepr.Repr(r.Profile), *r.PID, r.RestartVia))
			}
		}
		if r.PID != nil && e.ExternalGatewayPIDs[*r.PID] {
			return "external"
		}
		return "unaccounted"
	}
	for _, r := range plan.Runtimes {
		if r == nil {
			continue
		}
		var pid any
		if r.PID != nil {
			pid = int64(*r.PID)
		}
		row := pyjson.ObjectOf("kind", r.Kind, "profile", r.Profile, "pid", pid, "mechanism", r.RestartVia, "outcome", outcome(r))
		if r.Detail != nil {
			for _, key := range []string{"systemd_unit", "systemd_scope"} {
				if value, present := r.Detail.Get(key); present {
					row.Set(key, value)
				}
			}
		}
		outcomes = append(outcomes, row)
	}
	return outcomes
}

// Warns about every runtime the restart phase never touched and reports whether one exists
func reportUnaccountedRuntimes(w io.Writer, outcomes []*pyjson.Object) bool {
	field := func(o *pyjson.Object, key string) string { return pyrepr.Str(rowValue(o, key)) }
	is := func(o *pyjson.Object, key string, want string) bool { return pyrepr.Equal(rowValue(o, key), want) }
	isServe := func(o *pyjson.Object) bool {
		kind, isStr := rowValue(o, "kind").(string)
		return isStr && serveKinds[kind]
	}
	var manual, deferred, external, missed []*pyjson.Object
	for _, o := range outcomes {
		switch {
		case is(o, "outcome", "deferred") && is(o, "mechanism", "respawn-argv"):
			manual = append(manual, o)
		case is(o, "outcome", "deferred"):
			deferred = append(deferred, o)
		case is(o, "outcome", "external"):
			external = append(external, o)
		case is(o, "outcome", "unaccounted"):
			missed = append(missed, o)
		}
	}
	if len(manual) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  ⚠ Manual serve restarts deferred to their owner (reminders retained until the old processes exit):")
		for _, o := range manual {
			fmt.Fprintf(w, "    • %s [%s] pid %s: relaunch `icebath serve` / `icebath dashboard`, or reconnect Desktop for an SSH backend\n", field(o, "kind"), field(o, "profile"), field(o, "pid"))
		}
	}
	if len(deferred) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  ℹ Left to the Desktop app (still on pre-update code until it is relaunched):")
		for _, o := range deferred {
			action := "relaunch the Desktop app to pick up the update"
			if is(o, "mechanism", "desktop-ssh") {
				action = "owned by a Desktop connected over SSH; it picks up the update when that Desktop reconnects"
			}
			fmt.Fprintf(w, "    • %s [%s] pid %s — %s\n", field(o, "kind"), field(o, "profile"), field(o, "pid"), action)
		}
	}
	if len(external) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  ℹ Left to their own install (separate checkout, not restarted by this update):")
		for _, o := range external {
			fmt.Fprintf(w, "    • %s [%s] pid %s\n", field(o, "kind"), field(o, "profile"), field(o, "pid"))
		}
	}
	if len(missed) == 0 {
		return false
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  ⚠ Planned runtimes the restart phase never touched:")
	gatewayMissed, serveMissed, unitlessServe := false, false, false
	for _, o := range missed {
		fmt.Fprintf(w, "    ✗ %s [%s] pid %s — planned mechanism: %s\n", field(o, "kind"), field(o, "profile"), field(o, "pid"), field(o, "mechanism"))
		if isServe(o) {
			serveMissed = true
			unitlessServe = unitlessServe || !pyrepr.Truthy(rowValue(o, "systemd_unit"))
		} else {
			gatewayMissed = true
		}
	}
	fmt.Fprintln(w, "    Restart them manually, then verify:")
	if gatewayMissed {
		fmt.Fprintln(w, "      icebath gateway restart                # active profile")
		fmt.Fprintln(w, "      icebath -p <profile> gateway restart   # named profile")
	}
	if serveMissed {
		for _, o := range missed {
			if !isServe(o) || !pyrepr.Truthy(rowValue(o, "systemd_unit")) {
				continue
			}
			commands, err := gateway.SystemdRestartCommands(field(o, "systemd_unit"), rowValue(o, "systemd_scope"))
			if err != nil {
				panic(err)
			}
			for _, command := range commands {
				fmt.Fprintf(w, "      %s   # pid %s\n", command, field(o, "pid"))
			}
		}
		if unitlessServe {
			if runtime.GOOS == "linux" {
				fmt.Fprintln(w, "      systemctl --user restart icebath-serve.service   # unit-managed serve")
			}
			fmt.Fprintln(w, "      relaunch `icebath serve` / `icebath dashboard`")
		}
	}
	return true
}
