package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydeadline"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypwd"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
)

// Seconds apply waits for the default gateway to confirm serving every profile
const migrateServedWait = 90 * time.Second

// Seconds the compensator waits for a restored gateway to come up
var compensatorWait = 30 * time.Second

// Rollback wording when neither the default nor a secondary had a gateway
const nothingRecorded = "no gateway was recorded; nothing to restore"

// s6 legs the migration drives, assigned in cmd/icebath/wiring.go from pkg/gatewaycli
var MigrateS6 struct {
	Running         func() bool
	Scandir         func() string
	NamedSlotName   func(profile string) string
	SlotIsUp        func(profile string) bool
	ParkNamedSlot   func(ctx context.Context, profile string) error
	BringRootSlotUp func() (string, error)
}

// RuntimeError raised by a service step the boot enablement check reports instead of propagating
type migrateRuntimeError struct{ msg string }

func (e *migrateRuntimeError) Error() string { return e.msg }

// Whether the error is of the RuntimeError family a boot enablement failure reports
func isMigrateRuntimeError(err error) bool {
	var rt *migrateRuntimeError
	var unavailable gateway.SystemctlUnavailableError
	return errors.As(err, &rt) || errors.As(err, &unavailable)
}

// Whether the error is a process exit that no Exception handler may swallow
func isSystemExit(err error) bool {
	var exit *gateway.SystemExitError
	return errors.As(err, &exit)
}

func printLines(w io.Writer, lines []string) {
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
}

// Stop, uninstall, start, restart, install or enable on the home's service, the s6 slot leg under s6
var migrateServiceOp = func(ctx context.Context, w io.Writer, kind string, system bool, verb, h, runAsUser string) error {
	if kind == "s6" {
		return s6SlotOp(ctx, verb, h)
	}
	scoped, restore := HomeEnv(ctx, h)
	defer restore()
	switch verb {
	case "install":
		switch kind {
		case "launchd":
			return gateway.LaunchdInstall(scoped, w, false, true, false)
		case "windows":
			on := true
			return gateway.WindowsInstall(scoped, gateway.WindowsInstallOptions{StartNow: &on, StartOnLogin: &on})
		}
		return gateway.SystemdInstall(scoped, w, false, system, runAsUser, true, true, false)
	case "enable":
		if kind != "systemd" {
			return nil
		}
		name, err := gateway.GetServiceName(scoped)
		if err != nil {
			return err
		}
		enabled, err := migrateRunSystemctl(scoped, []string{"enable", name}, system, 30*time.Second)
		if err != nil {
			return err
		}
		if enabled.ReturnCode != 0 {
			return &migrateRuntimeError{fmt.Sprintf("could not enable %s at boot (systemctl enable exited %d); "+
				"a reboot would come up with no gateway", name, enabled.ReturnCode)}
		}
		return nil
	}
	return gateway.ServiceCall(scoped, kind, verb, system)
}

// systemctl runner and the root check the service legs call
var (
	migrateRunSystemctl = gateway.RunSystemctl
	migrateRequireRoot  = gateway.RequireRootForSystemService
)

// Profile name of a home, default for the root or an unknown home
func migrateProfileName(h string) string {
	if name, ok := gateway.ProfileNameForHome(h); ok && name != "" {
		return name
	}
	return "default"
}

// s6 leg, the root slot brought up on start or restart, a named slot parked down on stop
func s6SlotOp(ctx context.Context, verb, h string) error {
	name := migrateProfileName(h)
	if name == "default" {
		if verb == "start" || verb == "restart" {
			_, err := MigrateS6.BringRootSlotUp()
			return err
		}
		return nil
	}
	if verb == "stop" {
		return MigrateS6.ParkNamedSlot(ctx, name)
	}
	return nil
}

// Stops the gateway recorded in the home's pid file
var migrateStopGatewayProcess = func(h string) { manage.StopGatewayProcess(h) }

// Spawns a detached gateway for the home, false when the spawn failed
var migrateSpawnDetachedGateway = func(ctx context.Context, h string) (bool, error) {
	scoped, restore := HomeEnv(ctx, h)
	defer restore()
	return gateway.SpawnDetachedGateway(scoped)
}

// Every running gateway process apply will signal, in apply order
func signalledGateways(plan *MigrationPlan) []string {
	who := func(p *ProfileGateway, suffix string) string {
		where := "supervised by " + p.ServiceLabel()
		if p.PID != 0 {
			where = fmt.Sprintf("pid %d", p.PID)
		}
		return fmt.Sprintf("%s (%s%s)", p.Name, where, suffix)
	}
	signalled := []string{}
	for _, p := range plan.StandaloneSecondaries() {
		if p.PID != 0 || len(p.Services) > 0 {
			signalled = append(signalled, who(p, ""))
		}
	}
	if d := plan.Default(); len(d.Services) > 0 || d.PID != 0 {
		signalled = append(signalled, who(d, ", restarted onto the new flag"))
	}
	return signalled
}

// Plan table, the steps apply takes and every process it will signal, then blockers and notices
func FormatPlan(plan *MigrationPlan, dryRun bool) []string {
	head := "Migration plan"
	if dryRun {
		head = "Migration plan (dry run — nothing changed)"
	}
	lines := []string{head, "  default home: " + plan.DefaultHome, "", "  profile      gateway pid   service"}
	for _, p := range plan.Profiles {
		pid := "-"
		if p.PID != 0 {
			pid = fmt.Sprint(p.PID)
		}
		lines = append(lines, fmt.Sprintf("  %-12s %-13s %s", p.Name, pid, p.ServiceLabel()))
	}
	if len(plan.StandaloneByConfig) > 0 {
		lines = append(lines, "  Standalone by config (gateway.standalone: true), left alone: "+strings.Join(plan.StandaloneByConfig, ", "),
			"    (temporary compatibility shim; remove the key and re-run once the gaps it covers for you are fixed)")
	}
	lines = append(lines, "")
	if plan.AlreadyMultiplexed() {
		serving := " (flag on)"
		if len(plan.LiveServed) > 0 {
			serving = " (serving " + strings.Join(plan.LiveServed, ", ") + ")"
		}
		return append(lines, "  ✓ The default gateway is already multiplexing"+serving+".")
	}
	if plan.Interrupted {
		lines = append(lines, fmt.Sprintf("  ↻ An earlier migration was interrupted before the default gateway came up "+
			"(flag on, no live multiplexer; manifest %s); this run resumes it.", filepath.Join(plan.DefaultHome, MigrationManifestName)))
	} else if plan.MultiplexFlagOn && len(plan.StandaloneSecondaries()) > 0 {
		lines = append(lines, "  ↻ Half-migrated host: the flag is on, but the profile(s) below still own a "+
			"gateway. This run converges them.")
	}
	steps := []string{}
	signalled := signalledGateways(plan)
	for _, p := range plan.StandaloneSecondaries() {
		parts := []string{}
		if p.PID != 0 {
			parts = append(parts, fmt.Sprintf("stop pid %d", p.PID))
		}
		removals := make([]string, len(p.Services))
		for i, s := range p.Services {
			removals[i] = removeVerb(s) + " " + serviceUnitLabel(s)
		}
		if joined := strings.Join(removals, " + "); joined != "" {
			parts = append(parts, joined)
		}
		steps = append(steps, fmt.Sprintf("  - %s: %s", p.Name, strings.Join(parts, " + ")))
	}
	if len(plan.Profiles) < 2 {
		return append(lines, planTail(plan)...)
	}
	if len(steps) == 0 {
		lines = append(lines, "  No secondary profile runs its own gateway; the only step is turning the flag on:")
	} else {
		lines = append(append(lines, "  Steps:"), steps...)
	}
	lines = append(lines, "  - default: set gateway.multiplex_profiles: true in "+filepath.Join(plan.DefaultHome, "config.yaml"))
	target := plan.TargetServiceKind()
	if plan.Manifest != nil {
		target, _ = resumeTarget(plan)
	}
	verb, via := "start", " (detached)"
	if plan.Default().HasGateway() {
		verb = "restart"
	}
	if target != nil {
		via = " via " + target.Kind
	}
	lines = append(lines, fmt.Sprintf("  - default: %s the gateway%s, verify it serves %d profiles", verb, via, len(plan.ExpectedServedNames())),
		fmt.Sprintf("  - record the previous state in %s (used to undo a FAILED apply, and to resume this command after a crash)",
			filepath.Join(plan.DefaultHome, MigrationManifestName)))
	if len(signalled) > 0 {
		lines = append(lines, "", "  ⚠ This SIGTERMs running gateway process(es): "+strings.Join(signalled, ", ")+".",
			"    They drain in-flight turns and exit; their profiles are served by the host gateway afterwards.")
	}
	return append(lines, planTail(plan)...)
}

// Blockers, as refusals or as resume findings when a manifest exists, then notices
func planTail(plan *MigrationPlan) []string {
	lines := []string{}
	if len(plan.Blockers) > 0 {
		if plan.Manifest == nil {
			lines = append(lines, "", "  ✗ Blockers (fix these first, nothing will be changed):")
		} else {
			lines = append(lines, "",
				fmt.Sprintf("  ⚠ Blockers found, but a migration is already in progress on this host (manifest %s).",
					manifestPath(plan.DefaultHome)),
				"    This run RESUMES it and converges anyway — refusing would strand the profiles whose",
				"    gateways an earlier attempt already removed. Fix these afterwards:")
		}
		for _, b := range plan.Blockers {
			lines = append(lines, "    • "+b)
		}
	}
	if len(plan.Notices) > 0 {
		lines = append(lines, "", "  Notices:")
		for _, n := range plan.Notices {
			lines = append(lines, "    • "+n)
		}
	}
	return lines
}

// Secondary records of the manifest, false when a hand edited record lacks its profile or home
func manifestSecondaries(manifest *pyjson.Object) ([]*pyjson.Object, bool) {
	raw, present := manifest.Get("secondaries")
	if !present {
		return []*pyjson.Object{}, true
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	recs := make([]*pyjson.Object, 0, len(list))
	for _, item := range list {
		rec, ok := item.(*pyjson.Object)
		if !ok || !pyrepr.Truthy(rec.GetOr("profile", nil)) || !pyrepr.Truthy(rec.GetOr("home", nil)) {
			return nil, false
		}
		recs = append(recs, rec)
	}
	return recs, true
}

func serviceFromDict(service any) *ServiceUnit {
	rec, ok := service.(*pyjson.Object)
	if !ok || !pyrepr.Truthy(rec.GetOr("kind", nil)) {
		return nil
	}
	return &ServiceUnit{Kind: pyrepr.Str(rec.GetOr("kind", nil)), System: pyrepr.Truthy(rec.GetOr("system", nil))}
}

// Every service a record names, services when present else the single service of an older manifest
func recordedServices(rec *pyjson.Object) []ServiceUnit {
	if list, ok := rec.GetOr("services", nil).([]any); ok {
		out := []ServiceUnit{}
		for _, item := range list {
			if s := serviceFromDict(item); s != nil {
				out = append(out, *s)
			}
		}
		return out
	}
	if s := serviceFromDict(rec.GetOr("service", nil)); s != nil {
		return []ServiceUnit{*s}
	}
	return []ServiceUnit{}
}

func recordedRunAsUser(rec *pyjson.Object) string {
	user, _ := rec.GetOr("run_as_user", nil).(string)
	return user
}

// Default record then the secondary records that are dicts
func manifestRecords(manifest *pyjson.Object) []*pyjson.Object {
	recs := []*pyjson.Object{}
	if d, ok := manifest.GetOr("default", nil).(*pyjson.Object); ok {
		recs = append(recs, d)
	}
	secondaries, _ := manifestSecondaries(manifest)
	return append(recs, secondaries...)
}

// Service manager and User the resumed default install uses, read from the recorded footprint
func targetFromManifest(manifest *pyjson.Object) (*ServiceUnit, string) {
	recs := manifestRecords(manifest)
	var target *ServiceUnit
	for _, r := range recs {
		if services := recordedServices(r); len(services) > 0 {
			target = &services[0]
			break
		}
	}
	user := ""
	for _, r := range recs {
		if u := recordedRunAsUser(r); u != "" {
			user = u
			break
		}
	}
	return target, user
}

// Update hook block naming every blocker the automatic migration will not cross
func FormatUpdateWarning(plan *MigrationPlan, autoBlockers []string) []string {
	lines := []string{"⚠ Your profiles each run their own gateway. A single multiplexed gateway is the recommended",
		"  setup, but this install cannot be migrated automatically yet:"}
	for _, b := range append(append([]string{}, plan.Blockers...), autoBlockers...) {
		lines = append(lines, "    • "+b)
	}
	return append(lines, "  After fixing the above, run:  "+MigrateCommand,
		"  (`icebath update` will migrate automatically once nothing blocks it.)")
}

func manifestPath(defaultHome string) string {
	return filepath.Join(defaultHome, MigrationManifestName)
}

// Manifest on disk, nil when missing, unreadable, malformed or not an object
func readManifest(defaultHome string) *pyjson.Object {
	raw, err := os.ReadFile(manifestPath(defaultHome))
	if err != nil || !utf8.Valid(raw) {
		return nil
	}
	data, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\ufeff"))
	if err != nil {
		return nil
	}
	obj, _ := data.(*pyjson.Object)
	return obj
}

// Whether a live default has not yet recorded serving every unparked profile the manifest migrated
func manifestNotYetServed(manifest *pyjson.Object, liveServed []string) bool {
	if manifest == nil {
		return false
	}
	migrated := map[string]bool{"default": true}
	for _, r := range manifestRecords(manifest) {
		h := r.GetOr("home", nil)
		if pyrepr.Truthy(h) && profiles.ProfileIsParked(pyrepr.Str(h)) {
			continue
		}
		name := r.GetOr("profile", nil)
		if !pyrepr.Truthy(name) {
			name = "default"
		}
		migrated[pyrepr.Str(name)] = true
	}
	served := map[string]bool{}
	for _, s := range liveServed {
		served[s] = true
	}
	for name := range migrated {
		if !served[name] {
			return true
		}
	}
	return false
}

// Clears served_profiles and secondary platform rows from the default runtime record without restamping identity
func reconcileStandaloneRuntime(ctx context.Context, defaultHome string, secondaryNames map[string]bool) error {
	path := filepath.Join(defaultHome, "gateway_state.json")
	runtime := gateway.ReadRuntimeStatus(path)
	if runtime == nil {
		return nil
	}
	runtime.Set("served_profiles", []any{})
	if platforms, ok := runtime.GetOr("platforms", nil).(*pyjson.Object); ok {
		prefixes := []string{}
		for name := range secondaryNames {
			if name != "" {
				prefixes = append(prefixes, name+":")
			}
		}
		kept := pyjson.NewObject()
		for _, key := range platforms.Keys {
			owned := false
			for _, prefix := range prefixes {
				if strings.HasPrefix(key, prefix) {
					owned = true
					break
				}
			}
			if !owned {
				kept.Set(key, platforms.Vals[key])
			}
		}
		runtime.Set("platforms", kept)
	}
	return config.AtomicJSONWriteWith(ctx, path, runtime, pyjson.Separators(",", ":"))
}

// Polls the default's served record until it covers expected, the last record read on timeout
var migrateWaitForServed = func(ctx context.Context, defaultHome string, expected map[string]bool, timeout time.Duration) ([]string, bool, error) {
	deadline := time.Now().Add(timeout)
	var served []string
	recorded := false
	for time.Now().Before(deadline) {
		_, restore := HomeEnv(ctx, defaultHome)
		var err error
		served, recorded, err = gateway.RecordedServedProfiles(defaultHome)
		restore()
		if err != nil {
			return nil, false, err
		}
		if recorded && coversNames(served, expected) {
			return served, true, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return served, recorded, nil
}

func coversNames(served []string, expected map[string]bool) bool {
	have := map[string]bool{}
	for _, s := range served {
		have[s] = true
	}
	for name := range expected {
		if !have[name] {
			return false
		}
	}
	return true
}

// Sets gateway.multiplex_profiles in the default profile config
var migrateWriteMultiplexFlag = gateway.WriteMultiplexFlag

// Brings the default gateway up on the new flag through its unit, an installed target or a detached spawn
var migrateRestartDefault = func(ctx context.Context, w io.Writer, planDefault *ProfileGateway, target *ServiceUnit, defaultHome, runAsUser string) (string, error) {
	if s := planDefault.Service(); s != nil {
		if err := migrateServiceOp(ctx, w, s.Kind, s.System, "restart", defaultHome, ""); err != nil {
			return "", err
		}
		return "restarted the default gateway via " + s.Kind, nil
	}
	if target != nil {
		if err := migrateServiceOp(ctx, w, target.Kind, target.System, "install", defaultHome, runAsUser); err != nil {
			return "", err
		}
		if err := migrateServiceOp(ctx, w, target.Kind, target.System, "start", defaultHome, ""); err != nil {
			return "", err
		}
		return "installed and started the default gateway via " + target.Kind, nil
	}
	verb := "started"
	if planDefault.PID != 0 {
		verb = "restarted"
		migrateStopGatewayProcess(defaultHome)
	}
	spawned, err := migrateSpawnDetachedGateway(ctx, defaultHome)
	if err != nil {
		return "", err
	}
	if !spawned {
		return "", &migrateRuntimeError{"could not spawn the default gateway (detached)"}
	}
	return verb + " the default gateway (detached; no service manager was in use)", nil
}

// Enables the survivor unit at boot before anything is removed, the failure line when it cannot be
func enableDefaultAtBoot(ctx context.Context, w io.Writer, plan *MigrationPlan) (string, error) {
	s := plan.Default().Service()
	if s == nil {
		return "", nil
	}
	if err := migrateServiceOp(ctx, w, s.Kind, s.System, "enable", plan.DefaultHome, ""); err != nil {
		if isMigrateRuntimeError(err) {
			return "default: " + err.Error(), nil
		}
		return "", err
	}
	return "", nil
}

// Stops and removes every unit of each standalone secondary, then stops its detached process
func removeSecondaryGateways(ctx context.Context, w io.Writer, plan *MigrationPlan) error {
	for _, p := range plan.StandaloneSecondaries() {
		for _, s := range p.Services {
			if err := migrateServiceOp(ctx, w, s.Kind, s.System, "stop", p.Home, ""); err != nil {
				return err
			}
			if err := migrateServiceOp(ctx, w, s.Kind, s.System, "uninstall", p.Home, ""); err != nil {
				return err
			}
			done := "removed"
			if s.Kind == "s6" {
				done = "parked (down file)"
			}
			fmt.Fprintf(w, "  ✓ %s: stopped and %s its %s\n", p.Name, done, serviceUnitLabel(s))
		}
		if p.PID != 0 {
			migrateStopGatewayProcess(p.Home)
			fmt.Fprintf(w, "  ✓ %s: stopped standalone gateway (pid %d)\n", p.Name, p.PID)
		}
	}
	return nil
}

// Failure of the destructive phase knowable from the plan alone, refused before any gateway is stopped
var migratePreflightApply = func(ctx context.Context, plan *MigrationPlan, target *ServiceUnit, runAsUser string) (string, error) {
	if _, err := config.RequireReadableConfigBeforeWrite(ctx, filepath.Join(plan.DefaultHome, "config.yaml")); err != nil {
		return fmt.Sprintf("default: config.yaml cannot be updated (%s)", err.Error()), nil
	}
	systemTarget := target != nil && *target == ServiceUnit{"systemd", true}
	touchesSystemUnit := systemTarget
	for _, p := range plan.StandaloneSecondaries() {
		touchesSystemUnit = touchesSystemUnit || p.HasSystemUnit()
	}
	if touchesSystemUnit {
		if err := migrateRequireRoot("migration"); err != nil {
			return err.Error(), nil
		}
	}
	if plan.Default().Service() == nil && systemTarget {
		if runAsUser == "" {
			_, _, _, _, err := gateway.SystemServiceIdentity("")
			var refusal *gateway.ServiceUserError
			if errors.As(err, &refusal) {
				return "default: " + err.Error(), nil
			}
			if err != nil {
				return "", err
			}
		} else {
			_, err := pypwd.Getpwnam(runAsUser)
			var keyErr *pypwd.KeyError
			if errors.As(err, &keyErr) {
				return fmt.Sprintf("default: the recorded service user '%s' does not exist on this host", runAsUser), nil
			}
			if err != nil {
				return "", err
			}
		}
	}
	return "", nil
}

// Service manager and User an apply uses, the manifest's record winning on a resume
func resumeTarget(plan *MigrationPlan) (*ServiceUnit, string) {
	target, runAsUser := plan.TargetServiceKind(), plan.TargetRunAsUser()
	if plan.Manifest == nil {
		return target, runAsUser
	}
	mTarget, mUser := targetFromManifest(plan.Manifest)
	if mTarget != nil {
		target = mTarget
	}
	if mUser != "" {
		runAsUser = mUser
	}
	return target, runAsUser
}

// Every safety check re-run on a resume and reported as findings instead of refusals
func resumeFindings(ctx context.Context, plan *MigrationPlan, target *ServiceUnit, runAsUser string) ([]string, error) {
	findings := append([]string{}, plan.Blockers...)
	applyBlocker, err := migratePreflightApply(ctx, plan, target, runAsUser)
	if err != nil {
		return nil, err
	}
	if applyBlocker != "" {
		findings = append(findings, applyBlocker)
	}
	if len(findings) == 0 {
		return []string{}, nil
	}
	lines := []string{"  ⚠ Preflight findings — resuming anyway (this host is mid-migration; refusing would",
		"    leave its profiles with no gateway at all). Fix these once it is converged:"}
	for _, f := range findings {
		lines = append(lines, "    • "+f)
	}
	return lines, nil
}

// Flips the flag, removes every secondary gateway, brings the multiplexer up and verifies it serves every profile
func ApplyMigration(ctx context.Context, w io.Writer, plan *MigrationPlan, servedWait time.Duration) (bool, error) {
	if plan.AlreadyMultiplexed() {
		fmt.Fprintln(w, "✓ Already multiplexed — nothing to do.")
		return true, nil
	}
	target, runAsUser := resumeTarget(plan)
	if plan.Manifest != nil {
		fmt.Fprintf(w, "  ↻ resuming the migration recorded in %s\n", manifestPath(plan.DefaultHome))
		findings, err := resumeFindings(ctx, plan, target, runAsUser)
		if err != nil {
			return false, err
		}
		printLines(w, findings)
		enableFailure, err := enableDefaultAtBoot(ctx, w, plan)
		if err != nil {
			return false, err
		}
		if enableFailure != "" {
			fmt.Fprintf(w, "  ⚠ %s — resuming anyway\n", enableFailure)
		}
	} else {
		if plan.Blocked() {
			lines := []string{"✗ Migration refused:"}
			for _, b := range plan.Blockers {
				lines = append(lines, "  • "+b)
			}
			printLines(w, lines)
			return false, nil
		}
		blocker, err := migratePreflightApply(ctx, plan, target, runAsUser)
		if err != nil {
			return false, err
		}
		if blocker == "" {
			if blocker, err = enableDefaultAtBoot(ctx, w, plan); err != nil {
				return false, err
			}
		}
		if blocker != "" {
			printLines(w, []string{"✗ Migration refused before changing anything:", "  • " + blocker})
			return false, nil
		}
		secondaries := []any{}
		for _, p := range plan.StandaloneSecondaries() {
			secondaries = append(secondaries, p.ToDict())
		}
		manifest := pyjson.ObjectOf("version", int64(1), "migrated_at", time.Now().Format("2006-01-02T15:04:05-0700"),
			"flag_was", plan.MultiplexFlagOn, "default", plan.Default().ToDict(), "secondaries", secondaries)
		if err := config.AtomicJSONWrite(ctx, manifestPath(plan.DefaultHome), manifest); err != nil {
			return false, err
		}
	}
	err := func() error {
		if err := migrateWriteMultiplexFlag(ctx, plan.DefaultHome, true); err != nil {
			return err
		}
		fmt.Fprintf(w, "  ✓ default: gateway.multiplex_profiles: true (%s)\n", filepath.Join(plan.DefaultHome, "config.yaml"))
		if err := removeSecondaryGateways(ctx, w, plan); err != nil {
			return err
		}
		line, err := migrateRestartDefault(ctx, w, plan.Default(), target, plan.DefaultHome, runAsUser)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  ✓ %s\n", line)
		return nil
	}()
	if err != nil {
		if isSystemExit(err) {
			return false, err
		}
		printLines(w, []string{fmt.Sprintf("  ✗ migration failed (%s)", err.Error()),
			"  ↩ Restoring one host gateway so this host is not left without one..."})
		rolledBack, rbErr := RollbackMigration(ctx, w, plan.DefaultHome)
		if rbErr != nil {
			return false, rbErr
		}
		if !rolledBack {
			fmt.Fprintf(w, "  Re-run %s to resume from the manifest.\n", MigrateCommand)
		}
		return false, nil
	}
	expected := plan.ExpectedServedNames()
	served, recorded, err := migrateWaitForServed(ctx, plan.DefaultHome, expected, servedWait)
	if err != nil {
		return false, err
	}
	if recorded && coversNames(served, expected) {
		if err := os.Remove(manifestPath(plan.DefaultHome)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		lines := []string{"", fmt.Sprintf("✓ Migrated: the default gateway now serves %d profiles: %s", len(served), strings.Join(served, ", "))}
		for _, n := range plan.Notices {
			lines = append(lines, "  • "+n)
		}
		printLines(w, lines)
		return true, nil
	}
	have := map[string]bool{}
	for _, s := range served {
		have[s] = true
	}
	missing := []string{}
	for name := range expected {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	printLines(w, []string{"", "⚠ Migration applied, but the default gateway has not confirmed serving: " + strings.Join(missing, ", "),
		"  Check `icebath gateway status` and the gateway log; the flag and manifest are in place.",
		fmt.Sprintf("  Re-run %s once it is healthy — it resumes from the manifest.", MigrateCommand)})
	return false, nil
}

// Bounded wait for a gateway that is actually up for the home, zero on timeout
var migrateWaitForLiveGateway = func(h string, timeout time.Duration) int {
	return pydeadline.PollUntil(func() (int, bool) {
		pid := migrateLiveGatewayPid(h)
		return pid, pid != 0
	}, timeout, 500*time.Millisecond)
}

// Compensator of a failed apply, puts the default's one gateway back on the recorded flag
func RollbackMigration(ctx context.Context, w io.Writer, defaultHome string) (bool, error) {
	if defaultHome == "" {
		defaultHome = home.Root()
	}
	manifest := readManifest(defaultHome)
	if manifest == nil {
		fmt.Fprintf(w, "✗ No migration manifest at %s; nothing to restore.\n", manifestPath(defaultHome))
		return false, nil
	}
	incomplete := fmt.Sprintf("⚠ Compensation incomplete; manifest kept at %s.\n  Re-run %s — it resumes from the manifest.",
		manifestPath(defaultHome), MigrateCommand)
	secondaries, ok := manifestSecondaries(manifest)
	if !ok {
		fmt.Fprintf(w, "✗ Malformed secondary records in %s; fix or delete the manifest.\n", manifestPath(defaultHome))
		return false, nil
	}
	if err := migrateWriteMultiplexFlag(ctx, defaultHome, pyrepr.Truthy(manifest.GetOr("flag_was", false))); err != nil {
		if isSystemExit(err) {
			return false, err
		}
		fmt.Fprintf(w, "  ✗ default: could not restore gateway.multiplex_profiles (%s)\n", err.Error())
		fmt.Fprintln(w, incomplete)
		return false, nil
	}
	fmt.Fprintln(w, "  ✓ default: gateway.multiplex_profiles restored")
	services := []ServiceUnit{}
	if rec, ok := manifest.GetOr("default", nil).(*pyjson.Object); ok {
		services = recordedServices(rec)
	}
	if len(services) == 0 {
		installed, err := migrateInstalledServices(ctx, defaultHome)
		if err != nil {
			return false, err
		}
		services = installed
	}
	defaultGW := &ProfileGateway{Name: "default", Home: defaultHome, PID: ownGatewayPid(defaultHome, hostGatewayOwner()), Services: services}
	names := make([]string, len(secondaries))
	nameSet := map[string]bool{}
	for i, rec := range secondaries {
		names[i] = pyrepr.Str(rec.GetOr("profile", nil))
		nameSet[names[i]] = true
	}
	if err := reconcileStandaloneRuntime(ctx, defaultHome, nameSet); err != nil {
		if isSystemExit(err) {
			return false, err
		}
		fmt.Fprintf(w, "  ✗ default: could not clear multiplex-owned runtime status (%s)\n", err.Error())
		fmt.Fprintln(w, incomplete)
		return false, nil
	}
	fmt.Fprintln(w, "  ✓ default: cleared multiplex-owned runtime status")
	anyRecorded := false
	for _, rec := range secondaries {
		if pyrepr.Truthy(rec.GetOr("pid", nil)) || len(recordedServices(rec)) > 0 {
			anyRecorded = true
			break
		}
	}
	if !defaultGW.HasGateway() && !anyRecorded {
		if err := os.Remove(manifestPath(defaultHome)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		fmt.Fprintf(w, "✓ Nothing was running before this attempt (%s); the flag is restored.\n", nothingRecorded)
		return true, nil
	}
	target, runAsUser := targetFromManifest(manifest)
	line, err := migrateRestartDefault(ctx, w, defaultGW, target, defaultHome, runAsUser)
	if err != nil {
		if isSystemExit(err) {
			return false, err
		}
		fmt.Fprintf(w, "  ✗ default: could not bring the host gateway back up via the recorded service manager (%s); "+
			"falling back to a detached gateway\n", err.Error())
		spawned, spawnErr := migrateSpawnDetachedGateway(ctx, defaultHome)
		if spawnErr != nil {
			if isSystemExit(spawnErr) {
				return false, spawnErr
			}
			fmt.Fprintf(w, "  ✗ default: the detached fallback also failed (%s)\n", spawnErr.Error())
			spawned = false
		}
		if !spawned {
			fmt.Fprintln(w, incomplete)
			return false, nil
		}
		fmt.Fprintln(w, "  ✓ default: started the host gateway detached (no service manager)")
	} else {
		fmt.Fprintf(w, "  ✓ %s\n", line)
	}
	live := migrateWaitForLiveGateway(defaultHome, compensatorWait)
	if live == 0 {
		fmt.Fprintf(w, "  ✗ default: no gateway confirmed serving this host within %.0fs "+
			"(check `icebath gateway status` and the gateway log)\n", compensatorWait.Seconds())
		fmt.Fprintln(w, incomplete)
		return false, nil
	}
	if err := os.Remove(manifestPath(defaultHome)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	fmt.Fprintf(w, "✓ Compensated: one host gateway is running again (pid %d) on the recorded flag.\n", live)
	if len(names) > 0 {
		fmt.Fprintf(w, "  Per-profile gateways this attempt had already removed: %s. They are "+
			"NOT reinstalled — one gateway per host is the only supported topology — and the host "+
			"gateway serves them as soon as it multiplexes.\n  Run %s to finish converging.\n", strings.Join(names, ", "), MigrateCommand)
	}
	return true, nil
}

// Reason this host cannot be converged here, empty when it can
var migrateHostSupportsMigration = func() string {
	if !MigrateS6.Running() {
		return ""
	}
	slot := filepath.Join(MigrateS6.Scandir(), MigrateS6.NamedSlotName("default"))
	if info, err := os.Stat(slot); err == nil && info.IsDir() {
		return ""
	}
	return fmt.Sprintf("s6-supervised container without a root gateway slot (%s); "+
		"the container's boot registers it — restart the container.", slot)
}
