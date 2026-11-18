//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// One running or expected runtime on this machine
type RuntimeRecord struct {
	Kind        string
	Profile     string
	PID         *int
	Supervisor  string
	CodeSHA     *string
	CodeVersion any
	RestartVia  string
	Detail      *pyjson.Object
}

// Runtime fields as an ordered JSON object in declaration order
func (r *RuntimeRecord) ToDict() *pyjson.Object {
	out := pyjson.NewObject()
	out.Set("kind", r.Kind)
	out.Set("profile", r.Profile)
	if r.PID != nil {
		out.Set("pid", int64(*r.PID))
	} else {
		out.Set("pid", nil)
	}
	out.Set("supervisor", r.Supervisor)
	if r.CodeSHA != nil {
		out.Set("code_sha", *r.CodeSHA)
	} else {
		out.Set("code_sha", nil)
	}
	out.Set("code_version", r.CodeVersion)
	out.Set("restart_via", r.RestartVia)
	detail := r.Detail
	if detail == nil {
		detail = pyjson.NewObject()
	}
	out.Set("detail", detail)
	return out
}

// Install shape, profiles and runtimes seen before any mutation
type UpdatePlan struct {
	InstallMethod    string
	UpdatableInPlace bool
	UpdateMechanism  string
	ExpectedSHA      any
	ExpectedVersion  any
	Profiles         []any
	Runtimes         []*RuntimeRecord
}

// Empty plan with the defaults of an unprobed install
func NewUpdatePlan() *UpdatePlan {
	return &UpdatePlan{InstallMethod: "unknown", UpdatableInPlace: true, UpdateMechanism: "icebath update", Profiles: []any{}, Runtimes: []*RuntimeRecord{}}
}

// Plan fields as an ordered JSON object, every runtime as its own object
func (p *UpdatePlan) ToDict() *pyjson.Object {
	out := pyjson.NewObject()
	out.Set("install_method", p.InstallMethod)
	out.Set("updatable_in_place", p.UpdatableInPlace)
	out.Set("update_mechanism", p.UpdateMechanism)
	out.Set("expected_sha", p.ExpectedSHA)
	out.Set("expected_version", p.ExpectedVersion)
	out.Set("profiles", append([]any{}, p.Profiles...))
	runtimes := make([]any, len(p.Runtimes))
	for i, r := range p.Runtimes {
		runtimes[i] = r.ToDict()
	}
	out.Set("runtimes", runtimes)
	return out
}

// Field of a decoded object with whether the key is present
func planField(data any, key string) (any, bool) {
	value, present, _ := rowLookup(data, key)
	return value, present
}

// Plan rebuilt from its JSON form, unknown keys dropped and rows that are not objects skipped
func UpdatePlanFromDict(data any) (*UpdatePlan, error) {
	plan := NewUpdatePlan()
	typeError := func(field string, v any) error {
		return fmt.Errorf("update plan field %s is %s, which it cannot hold", field, errchain.Kind(v))
	}
	if v, ok := planField(data, "install_method"); ok {
		s, isStr := v.(string)
		if !isStr {
			return nil, typeError("install_method", v)
		}
		plan.InstallMethod = s
	}
	if v, ok := planField(data, "updatable_in_place"); ok {
		plan.UpdatableInPlace = pyrepr.Truthy(v)
	}
	if v, ok := planField(data, "update_mechanism"); ok {
		s, isStr := v.(string)
		if !isStr {
			return nil, typeError("update_mechanism", v)
		}
		plan.UpdateMechanism = s
	}
	if v, ok := planField(data, "expected_sha"); ok {
		plan.ExpectedSHA = v
	}
	if v, ok := planField(data, "expected_version"); ok {
		plan.ExpectedVersion = v
	}
	if v, ok := planField(data, "profiles"); ok {
		items, isList := v.([]any)
		if !isList {
			return nil, typeError("profiles", v)
		}
		plan.Profiles = append([]any{}, items...)
	}
	rows, _ := planField(data, "runtimes")
	if items, isList := rows.([]any); isList {
		for _, row := range items {
			if !isRowDict(row) {
				continue
			}
			record, err := runtimeRecordFromDict(row)
			if err != nil {
				return nil, err
			}
			plan.Runtimes = append(plan.Runtimes, record)
		}
	} else if pyrepr.Truthy(rows) {
		return nil, typeError("runtimes", rows)
	}
	return plan, nil
}

// Runtime rebuilt from its JSON form, a row lacking kind or profile refused like the missing constructor argument
func runtimeRecordFromDict(row any) (*RuntimeRecord, error) {
	record := &RuntimeRecord{Supervisor: "manual", Detail: pyjson.NewObject()}
	for _, required := range []string{"kind", "profile"} {
		value, present := planField(row, required)
		if !present {
			return nil, fmt.Errorf("a runtime record has no %s", required)
		}
		text := pyrepr.Str(value)
		if required == "kind" {
			record.Kind = text
		} else {
			record.Profile = text
		}
	}
	if v, ok := planField(row, "pid"); ok && v != nil {
		if n, isInt := exactInt(v); isInt {
			pid := int(n)
			record.PID = &pid
		}
	}
	if v, ok := planField(row, "supervisor"); ok {
		record.Supervisor = pyrepr.Str(v)
	}
	if v, ok := planField(row, "code_sha"); ok && v != nil {
		sha := pyrepr.Str(v)
		record.CodeSHA = &sha
	}
	if v, ok := planField(row, "code_version"); ok {
		record.CodeVersion = v
	}
	if v, ok := planField(row, "restart_via"); ok {
		record.RestartVia = pyrepr.Str(v)
	}
	if v, ok := planField(row, "detail"); ok {
		switch detail := v.(type) {
		case *pyjson.Object:
			record.Detail = detail
		case map[string]any:
			ordered := pyjson.NewObject()
			keys := make([]string, 0, len(detail))
			for key := range detail {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				ordered.Set(key, detail[key])
			}
			record.Detail = ordered
		}
	}
	return record, nil
}

var (
	inventoryServicePIDs         = gateway.GetServicePIDs
	inventoryWindowsServices     = gatewaycli.FindWindowsGatewayServices
	inventorySupportsSystemd     = gateway.SupportsSystemdServices
	inventoryIsMacOS             = gateway.IsMacOS
	inventoryProfileGatewayProcs = func(ctx context.Context) ([]gatewaycli.ProfileGatewayProcess, error) {
		return gatewaycli.FindProfileGatewayProcesses(ctx, nil, false)
	}
	inventoryReadImageProvenance = func() *ImageProvenance { return ReadImageProvenance("") }
	inventoryDetectInstallMethod = func(ctx context.Context) (string, error) {
		return config.DetectInstallMethod(ctx, home.Dir(ctx), fleetProjectRoot())
	}
	inventoryManagedSystem = func(ctx context.Context) string { return config.ManagedSystem(ctx, home.Dir(ctx)) }
)

// How a live gateway pid is supervised, from the service pid sets
func detectSupervisorForPid(ctx context.Context, pid int, servicePIDs, windowsServicePIDs map[int]bool) string {
	if len(windowsServicePIDs) > 0 && windowsServicePIDs[pid] {
		return "windows-service"
	}
	if !servicePIDs[pid] {
		return "manual"
	}
	return quietSupervisor(ctx)
}

// Service manager of this host, service when neither probe answers or one panics
func quietSupervisor(ctx context.Context) (name string) {
	defer func() {
		if r := recover(); r != nil {
			name = "service"
		}
	}()
	if inventorySupportsSystemd(ctx) {
		return "systemd"
	}
	if inventoryIsMacOS() {
		return "launchd"
	}
	return "service"
}

var restartMechanisms = map[string]string{"systemd": "systemd", "launchd": "launchd", "windows-service": "windows-service"}

var mechanismDescriptions = map[string]string{
	"systemd":         "systemctl restart (drain-first SIGUSR1 when supported)",
	"launchd":         "launchctl kickstart -k (drain-first, per-label domain)",
	"windows-service": "sc.exe stop before venv mutation, sc.exe start after update",
}

// Machine readable restart mechanism id of a supervisor, manual when the table has none
func restartMechanism(supervisor string) string {
	if id, ok := restartMechanisms[supervisor]; ok {
		return id
	}
	return "manual"
}

// Human readable description of a restart mechanism id, the gateway restart command when the table has none
func DescribeRestartMechanism(mechanism, profile string) string {
	if text := mechanismDescriptions[mechanism]; text != "" {
		return text
	}
	if profile != "default" {
		return "icebath -p " + profile + " gateway restart"
	}
	return "icebath gateway restart"
}

// Runtime record with restart_via derived from its supervisor
func newRuntime(kind, profile string, pid *int, supervisor string, codeSHA, codeVersion any, detail *pyjson.Object) *RuntimeRecord {
	record := &RuntimeRecord{Kind: kind, Profile: profile, PID: pid, Supervisor: supervisor, CodeVersion: codeVersion,
		RestartVia: restartMechanism(supervisor), Detail: detail}
	if record.Detail == nil {
		record.Detail = pyjson.NewObject()
	}
	if pyrepr.Truthy(codeSHA) {
		sha := pyrepr.Str(codeSHA)
		record.CodeSHA = &sha
	}
	return record
}

// Runs one inventory collector, a failure or panic logged at debug and yielding fewer rows
func inventoryProbe(label string, collect func() error) {
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Debug(fmt.Sprintf("%s failed: %v", label, r))
		}
	}()
	if err := collect(); err != nil {
		pkgLog().Debug(fmt.Sprintf("%s failed: %s", label, err))
	}
}

// Install method, in place updatability and update command of the plan, image provenance winning
func collectInstallShape(ctx context.Context, plan *UpdatePlan) {
	inventoryProbe("Install-method probe", func() error {
		method, err := inventoryDetectInstallMethod(ctx)
		if err != nil {
			return err
		}
		managed := inventoryManagedSystem(ctx)
		plan.InstallMethod = method
		if managed != "" {
			plan.InstallMethod = managed
		}
		plan.UpdatableInPlace = (method == "git" || method == "unknown") && managed == ""
		inventoryProbe("Image provenance probe", func() error {
			if provenance := inventoryReadImageProvenance(); provenance != nil {
				plan.UpdatableInPlace = false
				if provenance.Valid && provenance.Manager != "" {
					plan.InstallMethod = provenance.Manager
				}
			}
			return nil
		})
		plan.UpdateMechanism = config.RecommendedUpdateCommandForMethod(method)
		return nil
	})
}

// Classifier of a pid into its supervisor over the service pid sets, each probe degrading to an empty set
func supervisorClassifier(ctx context.Context) func(pid int) string {
	servicePIDs := map[int]bool{}
	inventoryProbe("Service-PID probe", func() error {
		pids, err := inventoryServicePIDs(ctx, true)
		if err != nil {
			return err
		}
		for _, pid := range pids {
			servicePIDs[pid] = true
		}
		return nil
	})
	windowsServicePIDs := map[int]bool{}
	inventoryProbe("Windows SCM service-ownership probe", func() error {
		services, err := inventoryWindowsServices(ctx)
		if err != nil {
			return err
		}
		for _, service := range services {
			windowsServicePIDs[service.GatewayPID] = true
		}
		return nil
	})
	return func(pid int) string { return detectSupervisorForPid(ctx, pid, servicePIDs, windowsServicePIDs) }
}

// Per profile gateways by control socket identity, then gateway_state.json, then pid file mapped gateways
func collectGatewayRuntimes(ctx context.Context, plan *UpdatePlan, profileHomes []profileHome, seen map[int]bool) {
	supervisor := supervisorClassifier(ctx)
	inventoryProbe("Gateway-state inventory", func() error {
		for _, ph := range profileHomes {
			var record *pyjson.Object
			var pid int
			var sup string
			if sockPID, identity, ok := fleetSocketIdentity(ph.home); ok {
				pid, record = sockPID, identity
				if seen[pid] {
					continue
				}
				seen[pid] = true
				if sup = gateway.LaunchSupervisor(record); sup == "" {
					sup = supervisor(pid)
				}
			} else {
				livePID, live, err := fleetLiveGatewayPID(ph.home)
				if err != nil {
					return err
				}
				if !live || seen[livePID] {
					continue
				}
				pid = livePID
				if record = fleetReadStatus(filepath.Join(ph.home, "gateway_state.json")); record == nil || record.Len() == 0 {
					record = pyjson.NewObject()
				}
				seen[pid] = true
				sup = supervisor(pid)
			}
			sha, _ := record.Get("code_sha")
			version, _ := record.Get("code_version")
			runtimePID := pid
			plan.Runtimes = append(plan.Runtimes, newRuntime("gateway", ph.profile, &runtimePID, sup, sha, version, nil))
		}
		return nil
	})
	inventoryProbe("PID-file gateway inventory", func() error {
		processes, err := inventoryProfileGatewayProcs(ctx)
		if err != nil {
			return err
		}
		for _, process := range processes {
			if !seen[process.PID] {
				seen[process.PID] = true
				pid := process.PID
				plan.Runtimes = append(plan.Runtimes, newRuntime("gateway", process.Profile, &pid, supervisor(pid), nil, nil, nil))
			}
		}
		return nil
	})
}

// Builds the plan read only, every collector degrading on its own
func CollectRuntimeInventory(ctx context.Context) *UpdatePlan {
	plan := NewUpdatePlan()
	collectInstallShape(ctx, plan)
	inventoryProbe("Code-identity probe", func() error {
		identity, err := fleetCodeIdentity(true)
		if err != nil {
			return err
		}
		plan.ExpectedSHA, _ = identity.Get("sha")
		plan.ExpectedVersion, _ = identity.Get("version")
		return nil
	})
	var profileHomes []profileHome
	inventoryProbe("Profile enumeration", func() error {
		profileHomes = fleetHomes()
		plan.Profiles = make([]any, len(profileHomes))
		for i, ph := range profileHomes {
			plan.Profiles[i] = ph.profile
		}
		return nil
	})
	seen := map[int]bool{}
	collectGatewayRuntimes(ctx, plan, profileHomes, seen)
	return plan
}
