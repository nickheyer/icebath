//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"fmt"
	"io"
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
	ExternalGatewayPIDs          map[int]bool
	LiveGatewayPIDs              map[string]map[int]bool
}

// Bare unit or label name of a service value, its service suffix and scope prefix dropped
func bareServiceName(service any) string {
	name := strings.TrimSuffix(pyrepr.Str(service), ".service")
	return name[strings.LastIndex(name, "/")+1:]
}

// True when service names the gateway unit, launchd label or s6 service of a profile exactly
func gatewayServiceMatchesProfile(profile string, service any) bool {
	name := bareServiceName(service)
	if profile == "default" {
		return name == "icebath-gateway" || name == gateway.LaunchdLabelBase || name == "gateway" || name == "gateway-default"
	}
	return name == "icebath-gateway-"+profile || name == gateway.LaunchdLabelBase+"-"+profile || name == "gateway-"+profile
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
	outcome := func(r *RuntimeRecord) string {
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
				pkgLog().Debug(fmt.Sprintf("%d planned gateway runtimes for profile %s still need a verdict — successor evidence cannot attribute the restart, leaving pid %d on the name path",
					pendingGateways[r.Profile], pyrepr.Repr(r.Profile), *r.PID))
			} else if len(live) == 0 {
				pkgLog().Debug(fmt.Sprintf("No post-restart gateway evidence for profile %s (planned pid %d via %s) — reconciling on restart bookkeeping alone",
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
		outcomes = append(outcomes, pyjson.ObjectOf("kind", r.Kind, "profile", r.Profile, "pid", pid, "mechanism", r.RestartVia, "outcome", outcome(r)))
	}
	return outcomes
}

// Warns about every runtime the restart phase never touched and reports whether one exists
func reportUnaccountedRuntimes(w io.Writer, outcomes []*pyjson.Object) bool {
	field := func(o *pyjson.Object, key string) string { return pyrepr.Str(rowValue(o, key)) }
	is := func(o *pyjson.Object, key string, want string) bool { return pyrepr.Equal(rowValue(o, key), want) }
	var external, missed []*pyjson.Object
	for _, o := range outcomes {
		switch {
		case is(o, "outcome", "external"):
			external = append(external, o)
		case is(o, "outcome", "unaccounted"):
			missed = append(missed, o)
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
	for _, o := range missed {
		fmt.Fprintf(w, "    ✗ %s [%s] pid %s — planned mechanism: %s\n", field(o, "kind"), field(o, "profile"), field(o, "pid"), field(o, "mechanism"))
	}
	fmt.Fprintln(w, "    Restart them manually, then verify:")
	fmt.Fprintln(w, "      icebath gateway restart                # active profile")
	fmt.Fprintln(w, "      icebath -p <profile> gateway restart   # named profile")
	return true
}
