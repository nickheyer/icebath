package selfupdate

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/discord"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyhost"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pysounddevice"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// doctorPython is what the managed runtime interpreter reports about itself, the counterpart of the sys module
type doctorPython struct {
	Version    [3]int
	Prefix     string
	BasePrefix string
	Path       []string
}

// doctorPackage is one library, program or runtime package an optional capability loads, probed the way that capability loads it
type doctorPackage struct {
	Name    string
	Present func(ctx context.Context) bool
}

const doctorPythonProbe = "import json, sys\nprint(json.dumps({'version': list(sys.version_info[:3]), 'prefix': sys.prefix, 'base_prefix': sys.base_prefix, 'path': sys.path}))\n"

var (
	doctorDetectInstallMethod  = config.DetectInstallMethod
	doctorInstallTrustStore    = provider.InstallTrustStore
	doctorModuleImportable     = pm.AnchorImportable
	doctorSystemdLingerStatus  = func(ctx context.Context) (*bool, string) { return gateway.GetSystemdLingerStatus(ctx, "") }
	doctorDetectServiceManager = gatewaycli.DetectServiceManager
	doctorIsLinux              = gateway.IsLinux
	doctorNewS6Manager         = func() doctorS6Manager { return gatewaycli.NewS6ServiceManager(gatewaycli.S6DynamicScandir) }
	doctorHostGatewayTopology  = gateway.CurrentHostGatewayTopology
	doctorHasLiveConnection    = state.HasLiveConnection
	doctorPathOnCrossVMFS      = state.PathOnCrossVMFS
	doctorResolveJournalMode   = state.ResolveJournalMode
	doctorForeignHolders       = state.ForeignStateDBHolders
	doctorDescribeHolderPID    = state.DescribeHolderPID
	doctorSelectedVenvDir      = func(ctx context.Context) (string, error) { return pm.SelectedVenv(ctx, pm.RepoRoot()) }
	doctorPackages             = []doctorPackage{
		{"PortAudio (voice mode audio)", func(context.Context) bool { return pysounddevice.Load() == nil }},
		{"Opus (Discord voice playback)", func(context.Context) bool { return discord.LoadOpusLibrary() == nil }},
		{"FFmpeg (audio conversion)", func(context.Context) bool { return media.FindFFmpegBinary() != "" }},
		{"Porcupine (wake word)", func(ctx context.Context) bool { return media.LoadPorcupineLibrary(ctx) == nil }},
		{"sherpa-onnx (wake word)", func(ctx context.Context) bool { return media.LoadSherpaOnnxLibrary(ctx) == nil }},
		{"openWakeWord (wake word)", func(ctx context.Context) bool { return media.LoadOpenWakeWordLibrary(ctx) == nil }},
		{"Python tool runtime (execute_code)", func(ctx context.Context) bool { _, err := codeexec.RuntimePython(ctx); return err == nil }},
		{"Python shared library (plugins, browser harness)", doctorPythonSharedLibraryPresent},
		{"browser-harness (browser_exec)", func(ctx context.Context) bool { return doctorModuleImportable(ctx, "browser_harness") }},
	}
)

// Whether the runtime interpreter the plugin host and browser harness embed has a loadable shared library
func doctorPythonSharedLibraryPresent(ctx context.Context) bool {
	python, err := codeexec.RuntimePython(ctx)
	if err != nil {
		return false
	}
	_, err = pyhost.Discover(python)
	return err == nil
}

// Default client TLS context, the error its construction raises when a trust bundle cannot be loaded
var doctorDefaultTLSContext = func() error {
	cfg := provider.PlatformTLSConfig()
	if cfg.RootCAs == nil && cfg.VerifyConnection != nil {
		return cfg.VerifyConnection(tls.ConnectionState{})
	}
	return nil
}

// Version, prefixes and import path the interpreter Icebath launches reports
var doctorRuntimePython = func(ctx context.Context) (*doctorPython, error) {
	root := pm.RepoRoot()
	venv, err := pm.SelectedVenv(ctx, root)
	if err != nil {
		return nil, err
	}
	command, err := pm.VenvCommand(ctx, root, venv, nil)
	if err != nil {
		return nil, err
	}
	done, err := doctorSubprocessRun(ctx, append(command, "-c", doctorPythonProbe), pysubprocess.Options{Timeout: 60 * time.Second, Check: true})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Version    [3]int   `json:"version"`
		Prefix     string   `json:"prefix"`
		BasePrefix string   `json:"base_prefix"`
		Path       []string `json:"path"`
	}
	if err := json.Unmarshal(done.Stdout, &payload); err != nil {
		return nil, err
	}
	return &doctorPython{Version: payload.Version, Prefix: payload.Prefix, BasePrefix: payload.BasePrefix, Path: payload.Path}, nil
}

// Repair instruction for a broken runtime that respects who owns the installation
func doctorPythonRepairHint(ctx context.Context, d *Doctor) (string, error) {
	method, err := doctorDetectInstallMethod(ctx, d.Home, d.ProjectRoot)
	if err != nil {
		return "", err
	}
	if config.IsNixInstallMethod(method) {
		return config.RecommendedUpdateCommandForMethod(method), nil
	}
	if method == "docker" || config.IsPackageManagerInstallMethod(method) {
		hint := "Run `" + config.RecommendedUpdateCommandForMethod(method) + "`"
		if method == "docker" {
			hint += ", then recreate the Icebath container"
		}
		return hint, nil
	}
	return "Run `icebath pm repair`, then restart Icebath", nil
}

// Install command of a system package for Termux, macOS or a Debian family host
func doctorSystemPackageInstallCmd(pkg string) string {
	manager := "sudo apt"
	if doctorIsTermux() {
		manager = "pkg"
	} else if doctorSysPlatform == "darwin" {
		manager = "brew"
	}
	return manager + " install " + pkg
}

// Actionable SQLite upgrade hint for an install method, the detected one when none is given
func doctorSQLiteUpgradeHint(ctx context.Context, d *Doctor, installMethod string) (string, error) {
	method := installMethod
	if method == "" {
		detected, err := doctorDetectInstallMethod(ctx, d.Home, d.ProjectRoot)
		if err != nil {
			return "", err
		}
		method = detected
	}
	cmd := config.RecommendedUpdateCommandForMethod(method)
	action := "run `icebath update`"
	switch {
	case config.IsNixInstallMethod(method):
		action = cmd
	case method == "docker":
		action = "run `" + cmd + "`, then recreate all Icebath containers"
	case config.IsPackageManagerInstallMethod(method):
		action = "run `" + cmd + "`"
	}
	return "(" + action + "; fixed versions: 3.51.3+ / 3.50.7 / 3.44.6 — see https://sqlite.org/wal.html#walresetbug)", nil
}

// doctorDatabase is the display name and path of one Icebath managed SQLite database
type doctorDatabase struct {
	Name, Path string
}

// Display name and path of every managed database of a home, the quick state stores then each kanban board
func doctorDatabasePaths(icebathHome string) []doctorDatabase {
	entries := []doctorDatabase{}
	for _, name := range backup.QuickStateFiles() {
		if strings.HasSuffix(name, ".db") {
			entries = append(entries, doctorDatabase{name, filepath.Join(icebathHome, filepath.FromSlash(name))})
		}
	}
	boards, _ := os.ReadDir(filepath.Join(icebathHome, "kanban", "boards"))
	names := []string{}
	for _, board := range boards {
		if _, err := os.Lstat(filepath.Join(icebathHome, "kanban", "boards", board.Name(), "kanban.db")); err == nil {
			names = append(names, board.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		rel := filepath.Join("kanban", "boards", name, "kanban.db")
		entries = append(entries, doctorDatabase{rel, filepath.Join(icebathHome, rel)})
	}
	return entries
}

const doctorSQLiteHeaderMagic = "SQLite format 3\x00"

// Why a database file could not be read, answered from metadata without opening it
func doctorUnreadableReason(dbPath string) string {
	if _, err := os.Stat(dbPath); err != nil {
		return pyfs.OSErrorText(err)
	}
	if pyfs.Access(dbPath, 4) {
		return "file could not be read"
	}
	return "permission denied: " + dbPath
}

// Journal mode from header byte 18 without opening the database, or the reason it could not be read
func doctorReadJournalMode(dbPath string) (string, string) {
	header, ok := state.ReadHeaderBytesPreopen(dbPath, 20, false)
	if !ok {
		if doctorHasLiveConnection(dbPath) {
			return "", "database is open in this process"
		}
		return "", doctorUnreadableReason(dbPath)
	}
	if len(header) == 0 {
		return "", "file is empty"
	}
	if len(header) < 20 || !strings.HasPrefix(string(header), doctorSQLiteHeaderMagic) {
		return "", "file is not a database"
	}
	switch header[18] {
	case 2:
		return "wal", ""
	case 1:
		return "rollback", ""
	}
	return "", fmt.Sprintf("unrecognized file-format version %d", header[18])
}

// Human readable size of a database file, size unknown when it cannot be read
func doctorFormatDBSize(dbPath string) string {
	info, err := os.Stat(dbPath)
	if err != nil {
		return "size unknown"
	}
	return tools.SizefmtBytes(info.Size())
}

// Names the processes holding a database or a sidecar, an incomplete scan warned as unproven quiet
func doctorReportDatabaseHolders(w io.Writer, name, dbPath string) error {
	holders, err := doctorForeignHolders(dbPath)
	if err != nil {
		return err
	}
	unknown := []string{}
	byPID := map[int]map[string]bool{}
	for _, holder := range holders {
		if holder.PID <= 0 || strings.HasPrefix(holder.Target, "uninspectable") {
			unknown = append(unknown, holder.Target)
			continue
		}
		if byPID[holder.PID] == nil {
			byPID[holder.PID] = map[string]bool{}
		}
		byPID[holder.PID][pypath.Basename(strings.TrimSuffix(holder.Target, " (deleted)"))] = true
	}
	pids := make([]int, 0, len(byPID))
	for pid := range byPID {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		targets := make([]string, 0, len(byPID[pid]))
		for target := range byPID[pid] {
			targets = append(targets, target)
		}
		sort.Strings(targets)
		doctorCheckInfo(w, fmt.Sprintf("%s is held by %s: %s", name, doctorDescribeHolderPID(pid), strings.Join(targets, ", ")))
	}
	switch {
	case len(unknown) > 0:
		first := []rune(unknown[0])
		if len(first) > 120 {
			first = first[:120]
		}
		detail := "(holder scan incomplete: " + string(first)
		if len(unknown) > 1 {
			detail += fmt.Sprintf("; +%d more", len(unknown)-1)
		}
		doctorCheckWarn(w, name+": cannot prove the database is quiet", detail+")")
	case len(byPID) == 0:
		doctorCheckInfo(w, name+": no other process holds it right now — the offline conversion can run")
	}
	return nil
}

// Lists the journal mode of each database, warning on WAL under a vulnerable SQLite or an unapplied delete setting
func doctorReportDatabaseJournalModes(ctx context.Context, d *Doctor, icebathHome string, versionInfo *state.VersionTuple) error {
	vulnerable := state.WALResetVulnerable(versionInfo)
	configured := doctorResolveJournalMode(ctx)
	if icebathHome == "" {
		icebathHome = d.Home
	}
	w := d.W
	exposed := []string{}
	for _, db := range doctorDatabasePaths(icebathHome) {
		name, path := db.Name, db.Path
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}
		mode, readErr := doctorReadJournalMode(path)
		size := doctorFormatDBSize(path)
		convert := "`icebath sessions set-journal-mode delete --db " + path + "`"
		if name == "state.db" {
			convert = "`icebath sessions set-journal-mode delete`"
		}
		crossVM := false
		if readErr == "" && mode == "wal" && configured != "delete" {
			onCrossVM, err := doctorPathOnCrossVMFS(path)
			if err != nil {
				return err
			}
			crossVM = onCrossVM
		}
		switch {
		case readErr == "" && mode == "wal" && configured == "delete":
			also := ""
			if vulnerable {
				exposed = append(exposed, name)
				also = "; also exposed to the WAL-reset bug"
			}
			doctorCheckWarn(w, fmt.Sprintf("%s is in WAL mode (%s) despite database.journal_mode=delete", name, size),
				"(the setting never applied: an existing WAL database is never live-downgraded"+also+
					". Stop every Icebath process for this profile, then run "+convert+")")
			if err := doctorReportDatabaseHolders(w, name, path); err != nil {
				return err
			}
		case readErr != "":
			if vulnerable {
				doctorCheckWarn(w, name+": journal mode could not be read", "("+readErr+"; cannot rule out WAL exposure)")
			} else {
				doctorCheckInfo(w, name+": journal mode could not be read ("+readErr+")")
			}
		case mode == "wal" && crossVM:
			if vulnerable {
				exposed = append(exposed, name)
			}
			doctorCheckWarn(w, fmt.Sprintf("%s is in WAL mode on a cross-VM filesystem (virtiofs/9p, %s)", name, size),
				"(WAL can silently corrupt across the VM boundary; stop every Icebath process and run "+convert+", then "+
					"set `database.journal_mode: delete` — or move the database onto a native/named volume)")
		case mode == "wal" && vulnerable:
			exposed = append(exposed, name)
			doctorCheckWarn(w, fmt.Sprintf("%s is in WAL mode (%s)", name, size), "(exposed to the WAL-reset bug until SQLite is upgraded)")
		case mode == "wal":
			doctorCheckInfo(w, fmt.Sprintf("%s: WAL journal mode (%s)", name, size))
		default:
			notExposed := ""
			if vulnerable {
				notExposed = ", not exposed"
			}
			doctorCheckInfo(w, fmt.Sprintf("%s: rollback journal mode (%s%s)", name, size, notExposed))
		}
	}
	if len(exposed) > 0 {
		doctorCheckInfo(w, "To clear the exposure: "+state.WALResetRepairHint(ctx))
	}
	return nil
}

// Reports the static s6 services and the one host gateway slot under an s6 init, silent elsewhere
func doctorCheckS6Supervision(ctx context.Context, d *Doctor, issues *[]string) error {
	kind, err := doctorDetectServiceManager()
	if err != nil {
		return err
	}
	if kind != "s6" {
		return nil
	}
	doctorSection(d.W, "s6 Supervision")
	mgr := doctorNewS6Manager()
	for _, static := range []string{"main-icebath", "dashboard"} {
		up, err := mgr.IsRunning(static)
		if err != nil {
			return err
		}
		if up {
			doctorCheckOK(d.W, static+": up", "")
		} else {
			doctorCheckInfo(d.W, static+": down (expected if not enabled via env)")
		}
	}
	return doctorReportHostGatewaySlot(d.W, mgr, issues)
}

// doctorS6Manager is the supervision surface the host gateway slot report reads
type doctorS6Manager interface {
	IsRunning(name string) (bool, error)
	ListProfileGateways() ([]string, error)
}

// Reports the one host gateway process and its roster, or the supervision slots nothing serves
func doctorReportHostGatewaySlot(w io.Writer, mgr doctorS6Manager, issues *[]string) error {
	slots, err := mgr.ListProfileGateways()
	if err != nil {
		return err
	}
	slots = append([]string{}, slots...)
	sort.Strings(slots)
	topology, err := doctorHostGatewayTopology()
	if err != nil {
		return err
	}
	if topology == nil {
		if len(slots) == 0 {
			doctorCheckInfo(w, "No gateway registered yet — run `icebath gateway install`")
			return nil
		}
		up := 0
		for _, p := range slots {
			running, err := mgr.IsRunning("gateway-" + p)
			if err != nil {
				return err
			}
			if running {
				up++
			}
		}
		*issues = append(*issues, "No host gateway owns the gateway role — start the ONE host multiplexer: "+
			"icebath --profile default gateway start")
		doctorCheckWarn(w, fmt.Sprintf("No host gateway owns the gateway role (%d/%d supervision slots up: %s)", up, len(slots), strings.Join(slots, ", ")),
			"(nothing is serving these profiles)")
		return nil
	}
	doctorCheckOK(w, "Host gateway: "+topology.Describe(), "")
	legacyUp := []string{}
	for _, p := range slots {
		if p == "default" {
			continue
		}
		running, err := mgr.IsRunning("gateway-" + p)
		if err != nil {
			return err
		}
		if running {
			legacyUp = append(legacyUp, p)
		}
	}
	if len(legacyUp) > 0 {
		doctorCheckWarn(w, "LEGACY per-profile gateway slots still supervised: "+strings.Join(legacyUp, ", "),
			"(multiplex-only: the host gateway already serves every profile from one process)")
		*issues = append(*issues, "Fold the legacy per-profile gateways into the host gateway: "+
			"icebath --profile default gateway migrate --multiplex")
	}
	return nil
}

// Verifies the platform trust store is in force and a default TLS context constructs
func doctorCheckCertificatesRows(ctx context.Context, d *Doctor, shouldFix bool, issues *[]string) error {
	platformStore := doctorInstallTrustStore()
	if !platformStore {
		doctorCheckWarn(d.W, "TLS platform trust store unavailable",
			"OpenSSL default trust paths are in use; certificates trusted only by the OS store (a corporate root) will not verify")
	}
	if err := doctorDefaultTLSContext(); err != nil {
		hint, hintErr := doctorPythonRepairHint(ctx, d)
		if hintErr != nil {
			return hintErr
		}
		doctorFailAndIssue(d.W, "TLS default SSL context cannot be constructed", err.Error(),
			hint+"; if TLS still fails, repair Python through the installation owner.", issues)
		return nil
	}
	if platformStore {
		doctorCheckOK(d.W, "TLS platform trust store configured; default SSL context available", "")
	} else {
		doctorCheckOK(d.W, "TLS default SSL context available (OpenSSL trust paths)", "")
	}
	return nil
}

// Warns when a systemd user gateway service will stop after logout, silent under s6 and off Linux
func doctorCheckGatewayServiceLinger(ctx context.Context, d *Doctor, issues *[]string) error {
	if !doctorIsLinux() {
		return nil
	}
	kind, err := doctorDetectServiceManager()
	if err != nil {
		return err
	}
	if kind == "s6" {
		return nil
	}
	hostUnit := filepath.Join(gateway.UserSystemdUnitDir(ctx), gateway.ServiceBase+".service")
	unitPath, err := gateway.GetSystemdUnitPath(ctx, false)
	if err != nil {
		return err
	}
	if !doctorPlatformPathExists(unitPath) && !doctorPlatformPathExists(hostUnit) {
		return nil
	}
	doctorSection(d.W, "Gateway Service")
	lingerEnabled, lingerDetail := doctorSystemdLingerStatus(ctx)
	if lingerEnabled == nil {
		doctorCheckWarn(d.W, "Could not verify systemd linger", "("+lingerDetail+")")
		return nil
	}
	if !doctorCheckBool(d.W, *lingerEnabled, doctorRow{"Systemd linger enabled", "(gateway service survives logout)"},
		doctorRow{"Systemd linger disabled", "(gateway may stop after logout)"}, false) {
		doctorCheckInfo(d.W, "Run: sudo loginctl enable-linger $USER")
		*issues = append(*issues, "Enable linger for the gateway user service: sudo loginctl enable-linger $USER")
	}
	return nil
}

// Reports whether a path exists, following symlinks
func doctorPlatformPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

const doctorTCCCdhashDetail = "the desktop bundle's designated requirement is cdhash-pinned (pre-#73681 build) — rebuilds invalidate " +
	"all permission grants. Run `icebath update` to get the stable identifier-pinned signing identity, " +
	"then re-grant permissions once."

var doctorTCCStableDetail = map[bool]string{
	true:  "(certificate-anchored DR; grants survive rebuilds)",
	false: "(identifier-pinned DR; grants survive rebuilds — for the strongest anchor, see `icebath desktop --setup-tcc-identity`)",
}

// Reports whether TCC grants of a locally built desktop bundle survive rebuilds, silent off macOS or without a bundle
func doctorCheckMacosTCCGrants(ctx context.Context, d *Doctor) {
	app := ""
	if doctorSysPlatform == "darwin" {
		app = doctorDesktopAppBundle(d)
	}
	if app == "" {
		return
	}
	dr, ok := doctorMacosDesktopDR(ctx, app)
	if !ok || dr == "" {
		doctorCheckWarn(d.W, "macOS TCC grant check", "(could not read code-signing requirement of the desktop bundle)")
		return
	}
	if strings.Contains(strings.ToLower(dr), "cdhash") {
		doctorCheckWarn(d.W, "macOS TCC grants will reset after every update", doctorTCCCdhashDetail)
		return
	}
	doctorCheckOK(d.W, "macOS TCC signing identity is stable", doctorTCCStableDetail[strings.Contains(strings.ToLower(dr), "certificate")])
	doctorCheckInfo(d.W, "If macOS still re-prompts for permissions (toggle shows ON): the stored grant is stale — run "+
		"`tccutil reset ScreenCapture ai.icebath` (repeat per affected service), toggle it ON in "+
		"System Settings, then fully quit & relaunch Icebath once.")
}

// Newest locally built desktop bundle under apps/desktop/release, empty when none exists
var doctorDesktopAppBundle = func(d *Doctor) string {
	matches, _ := filepath.Glob(filepath.Join(d.ProjectRoot, "apps", "desktop", "release", "mac*", "Icebath.app"))
	best, bestTime := "", time.Time{}
	for _, candidate := range matches {
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			best, bestTime = candidate, info.ModTime()
		}
	}
	return best
}

// Designated requirement text of a bundle, false when codesign is missing, fails or hangs
var doctorMacosDesktopDR = func(ctx context.Context, app string) (string, bool) {
	codesign := doctorWhich("codesign")
	if codesign == "" {
		return "", false
	}
	done, err := doctorSubprocessRun(ctx, []string{codesign, "-d", "--requirements", "-", app}, pysubprocess.Options{Timeout: 15 * time.Second})
	if err != nil || done.Returncode != 0 {
		return "", false
	}
	return strings.ToValidUTF8(string(done.Stdout), "�") + strings.ToValidUTF8(string(done.Stderr), "�"), true
}

// Reports the interpreter TCC anchor and installs it with fix, silent off macOS and for unmanaged interpreters
func doctorCheckMacosTCCAnchor(ctx context.Context, d *Doctor, shouldFix bool) error {
	return doctorWarnOnError(d.W, "macOS TCC anchor check failed", "", nil, func() error {
		status, detail, err := macosTCCAnchorState(ctx, d.ProjectRoot)
		if err != nil {
			return err
		}
		switch status {
		case "skip":
			return nil
		case "active":
			doctorCheckOK(d.W, "macOS TCC anchor active", "("+detail+")")
			return nil
		}
		if shouldFix {
			anchored, err := ensureMacosTCCAnchor(ctx, d.ProjectRoot)
			if err != nil {
				return err
			}
			if anchored != "" {
				doctorCheckOK(d.W, "macOS TCC anchor installed", "("+anchored+")")
				return nil
			}
		}
		if status == "missing" {
			doctorCheckWarn(d.W, "macOS TCC anchor missing", "("+detail+")")
		} else {
			doctorCheckWarn(d.W, "macOS TCC anchor stale", "("+detail+")")
		}
		return nil
	})
}

// Reports compromised package advisories as manual issues with their remediation text
func checkSecurityAdvisories(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheckGuarded(d.W, "Security advisory check failed: {e}", "", func(f *Finding) error {
		allHits := plugins.DetectCompromised(plugins.Advisories)
		freshHits := plugins.FilterUnackedAdvisories(ctx, allHits)
		if len(freshHits) == 0 {
			doctorCheckOK(d.W, "No active security advisories", "")
			return nil
		}
		c := &cliui.Console{Out: d.W}
		for _, hit := range freshHits {
			doctorFailAndIssue(d.W, hit.Advisory.Title, "("+hit.Package+"=="+hit.InstalledVersion+")",
				"Resolve security advisory "+hit.Advisory.ID+": uninstall "+hit.Package+"=="+hit.InstalledVersion+
					" and rotate credentials, then run `icebath doctor --ack "+hit.Advisory.ID+"`.", &f.ManualIssues)
			lines, err := plugins.AdvisoryFullRemediationText(ctx, hit)
			if err != nil {
				return err
			}
			for _, line := range lines {
				if line == "" {
					fmt.Fprintln(d.W)
				} else {
					fmt.Fprintln(d.W, "    "+c.Color(line, cliui.Yellow))
				}
			}
		}
		ackedIDs := plugins.GetAckedAdvisoryIDs(ctx)
		for _, h := range allHits {
			if ackedIDs[h.Advisory.ID] {
				doctorCheckWarn(d.W, h.Package+"=="+h.InstalledVersion+" still installed (advisory "+h.Advisory.ID+" acknowledged)", "")
			}
		}
		return nil
	})
}

// Provisioned runtime venv of PM, empty when nothing is staged or PM cannot say
var doctorStagedVenvDir = func(ctx context.Context) (staged string) {
	defer func() {
		if recover() != nil {
			staged = ""
		}
	}()
	venvDir, err := doctorSelectedVenvDir(ctx)
	if err != nil || venvDir == "" {
		return ""
	}
	if info, err := os.Stat(filepath.Join(venvDir, "pyvenv.cfg")); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return venvDir
}

// Compares a version triple with a major and minor pair
func doctorVersionLess(v [3]int, major, minor int) bool {
	return v[0] < major || (v[0] == major && v[1] < minor)
}

// Reports the interpreter, the linked SQLite, the runtime venv and the macOS TCC anchors and grants
func checkPythonEnvironment(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		py, err := doctorRuntimePython(ctx)
		if err != nil {
			hint, hintErr := doctorPythonRepairHint(ctx, d)
			if hintErr != nil {
				return hintErr
			}
			doctorFailAndIssue(d.W, "Python", "(missing)", "Repair Python: "+hint, &f.Issues)
			py = &doctorPython{}
		} else if v, label := py.Version, fmt.Sprintf("Python %d.%d.%d", py.Version[0], py.Version[1], py.Version[2]); doctorVersionLess(v, 3, 8) {
			doctorFailAndIssue(d.W, label, "(3.10+ required)", "Upgrade Python to 3.10+", &f.Issues)
		} else if doctorCheckBool(d.W, !doctorVersionLess(v, 3, 10), doctorRow{Text: label}, doctorRow{label, "(3.10+ recommended)"}, false) && doctorVersionLess(v, 3, 11) {
			doctorCheckWarn(d.W, "Python 3.11+ recommended for RL Training tools (tinker requires >= 3.11)", "")
		}
		if err := doctorWarnOnError(d.W, "SQLite version probe failed: {e}", "", nil, func() error {
			src := state.SQLiteSourceID()
			version := state.LinkedSQLiteVersion()
			hint, err := doctorSQLiteUpgradeHint(ctx, d, "")
			if err != nil {
				return err
			}
			doctorCheckBool(d.W, !state.WALResetVulnerable(nil), doctorRow{Text: "SQLite " + version},
				doctorRow{"SQLite " + version + " (WAL-reset bug)", hint}, false)
			if src != "" {
				if runes := []rune(src); len(runes) > 48 {
					src = string(runes[:48]) + "…"
				}
				doctorCheckInfo(d.W, "SQLite source id: "+src)
			}
			return doctorReportDatabaseJournalModes(ctx, d, "", nil)
		}); err != nil {
			return err
		}
		if staged := doctorStagedVenvDir(ctx); staged != "" {
			site, err := pm.SitePackages(staged)
			if err != nil {
				return err
			}
			selectedSite, err := pypath.Resolve(site)
			if err != nil {
				return err
			}
			runningHere := false
			if info, statErr := os.Stat(selectedSite); statErr == nil && info.IsDir() {
				for _, entry := range py.Path {
					resolved, err := pypath.Resolve(entry)
					if err != nil {
						return err
					}
					if resolved == selectedSite {
						runningHere = true
						break
					}
				}
			}
			detail := "(this process runs outside it)"
			if runningHere {
				detail = "(active in this process)"
			}
			doctorCheckOK(d.W, "Runtime venv staged ("+staged+")", detail)
		} else {
			doctorCheckBool(d.W, py.Prefix != py.BasePrefix, doctorRow{Text: "Virtual environment active"},
				doctorRow{"Not in virtual environment", "(recommended)"}, false)
		}
		if err := doctorCheckMacosTCCAnchor(ctx, d, shouldFix); err != nil {
			return err
		}
		if err := CheckMacosFullDiskAccess(d.W); err != nil {
			return err
		}
		doctorCheckMacosTCCGrants(ctx, d)
		return nil
	})
}

// Verifies the TLS policy, recording an unusable default context as a manual issue
func checkCertificates(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		return doctorCheckCertificatesRows(ctx, d, shouldFix, &f.ManualIssues)
	})
}

// Reports each library, program and runtime package an optional capability needs as present or not installed
func checkRequiredPackages(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		for _, p := range doctorPackages {
			if p.Present(ctx) {
				doctorCheckOK(d.W, p.Name, "(optional)")
			} else {
				doctorCheckWarn(d.W, p.Name, "(optional, not installed)")
			}
		}
		return nil
	})
}

var (
	doctorRedundantAutostartEntries   = gateway.RedundantAutostartEntries
	doctorReconcileAutostartLaunchers = gateway.ReconcileAutostartLaunchers
)

// Reports duplicate Windows gateway logon launchers and removes them with fix, silent elsewhere
func doctorCheckWindowsGatewayAutostart(ctx context.Context, d *Doctor, shouldFix bool, f *Finding) error {
	if doctorSysPlatform != "windows" {
		return nil
	}
	redundant, err := doctorRedundantAutostartEntries(ctx)
	if err != nil {
		return err
	}
	if len(redundant) == 0 {
		return nil
	}
	doctorSection(d.W, "Windows Gateway Autostart")
	if !shouldFix {
		for _, path := range redundant {
			doctorCheckWarn(d.W, "Redundant gateway login item", "("+path+")")
		}
		f.Issues = append(f.Issues, "Remove duplicate Windows gateway autostart entries: icebath doctor --fix")
		return nil
	}
	done, warnings, err := doctorReconcileAutostartLaunchers(ctx)
	if err != nil {
		return err
	}
	for _, message := range done {
		doctorCheckOK(d.W, message, "")
	}
	for _, message := range warnings {
		doctorCheckWarn(d.W, message, "")
	}
	if len(done) > 0 && len(warnings) == 0 {
		f.Fixed++
	}
	if len(warnings) > 0 {
		f.ManualIssues = append(f.ManualIssues, warnings...)
	}
	return nil
}

// Argv that loads the dashboard web surface in a child process and exits
var doctorDashboardProbeArgv = dashboardImportProbeArgv

// Loads the dashboard web surface in a child process so a crash on load lands in the report
func checkWebDashboardImport(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		argv, err := doctorDashboardProbeArgv(ctx, d.ProjectRoot)
		if err != nil {
			return err
		}
		env := []string{}
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, pm.EnvDisableLazyInstalls+"=") {
				env = append(env, entry)
			}
		}
		env = append(env, pm.EnvDisableLazyInstalls+"=1")
		repair := "Repair the dashboard dependencies: `icebath pm repair`, then restart Icebath"
		done, err := doctorSubprocessRun(ctx, argv, pysubprocess.Options{Dir: d.ProjectRoot, Env: env, Timeout: 120 * time.Second})
		if err != nil {
			var expired *pysubprocess.TimeoutExpired
			if !errors.As(err, &expired) {
				return err
			}
			doctorFailAndIssue(d.W, "Dashboard web surface", "(import probe timed out)", repair, &f.Issues)
			return nil
		}
		stderr := pystr.Strip(strings.ToValidUTF8(string(done.Stderr), "\ufffd"))
		switch {
		case done.Returncode == 0:
			doctorCheckOK(d.W, "Dashboard web surface", "(imports cleanly)")
		case strings.Contains(stderr, "Web UI requires fastapi and uvicorn"):
			doctorCheckWarn(d.W, "Dashboard web surface", "(optional web extra not installed)")
		default:
			detail := fmt.Sprintf("(exited with code %d)", done.Returncode)
			if stderr != "" {
				lines := pystr.Splitlines(stderr, false)
				detail = lines[len(lines)-1]
			}
			doctorFailAndIssue(d.W, "Dashboard web surface", detail, repair, &f.Issues)
		}
		return nil
	})
}

// Reports systemd linger, s6 supervision and Windows autostart of the gateway
func checkGatewaySupervision(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		if err := doctorCheckGatewayServiceLinger(ctx, d, &f.Issues); err != nil {
			return err
		}
		if err := doctorCheckS6Supervision(ctx, d, &f.Issues); err != nil {
			return err
		}
		return doctorCheckWindowsGatewayAutostart(ctx, d, shouldFix, f)
	})
}

// Reports whether a path is a symbolic link
func doctorIsSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&fs.ModeSymlink != 0
}

// Reports whether a PM failure is one the checks report as a row, lookup and type errors still propagating
func doctorPMErrorReported(err error) bool {
	var unknown *pm.UnknownPackageError
	if errors.As(err, &unknown) {
		return false
	}
	var pmErr *pm.PyError
	if errors.As(err, &pmErr) {
		switch pmErr.Class {
		case "KeyError", "LookupError", "IndexError", "TypeError", "AttributeError", "AssertionError":
			return false
		}
	}
	return true
}

// Checks the install owned command link and repairs an owned or missing one with fix
func checkCommandInstallation(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		if doctorSysPlatform == "windows" {
			return nil
		}
		w := d.W
		doctorSection(w, "Command Installation")
		method, err := doctorDetectInstallMethod(ctx, d.Home, d.ProjectRoot)
		if err != nil {
			return err
		}
		if config.IsNixInstallMethod(method) || method == "docker" || config.IsPackageManagerInstallMethod(method) {
			if command := doctorWhich("icebath"); command != "" {
				doctorCheckOK(w, "Icebath command managed by "+method+" ("+command+")", "")
				return nil
			}
			doctorCheckWarn(w, "Icebath command not on PATH ("+method+"-managed)", "")
			hint, err := doctorPythonRepairHint(ctx, d)
			if err != nil {
				return err
			}
			f.ManualIssues = append(f.ManualIssues, hint)
			return nil
		}
		selected, err := pm.SelectedVenv(ctx, d.ProjectRoot)
		if err != nil {
			if !doctorPMErrorReported(err) {
				return err
			}
			doctorCheckFail(w, "Cannot resolve selected dependencies", err.Error())
			hint, err := doctorPythonRepairHint(ctx, d)
			if err != nil {
				return err
			}
			f.ManualIssues = append(f.ManualIssues, hint)
			return nil
		}
		base, err := pm.BaseVenv(d.ProjectRoot)
		if err != nil {
			return err
		}
		storePython, err := pm.ResolveStorePython(ctx, d.ProjectRoot)
		if err != nil {
			return err
		}
		pmLauncher := selected != base || storePython != ""
		venvBin := filepath.Join(selected, "bin", "icebath")
		if pmLauncher {
			venvBin = filepath.Join(d.ProjectRoot, "icebath")
		}
		if info, err := os.Stat(venvBin); err != nil || !info.Mode().IsRegular() {
			doctorCheckWarn(w, "Icebath entry point not found", "("+venvBin+")")
			f.ManualIssues = append(f.ManualIssues, "Repair or reinstall the Icebath launcher through the installation owner")
			return nil
		}
		doctorCheckOK(w, "Icebath entry point exists ("+venvBin+")", "")
		prefix := os.Getenv("PREFIX")
		termux := prefix != "" && (os.Getenv("TERMUX_VERSION") != "" || strings.Contains(prefix, "com.termux/files/usr"))
		var linkDir, display string
		if termux {
			linkDir, display = filepath.Join(prefix, "bin"), "$PREFIX/bin"
		} else {
			userHome, err := doctorUserHomeDir()
			if err != nil {
				return err
			}
			linkDir, display = filepath.Join(userHome, ".local", "bin"), "~/.local/bin"
		}
		link := filepath.Join(linkDir, "icebath")
		verb := ""
		switch {
		case doctorIsSymlink(link):
			target, err := pypath.Resolve(link)
			if err != nil {
				return err
			}
			expected, err := pypath.Resolve(venvBin)
			if err != nil {
				return err
			}
			if target == expected {
				doctorCheckOK(w, display+"/icebath → correct target", "")
				return nil
			}
			doctorCheckWarn(w, display+"/icebath points to wrong target", "(→ "+target+", expected → "+expected+")")
			owned := false
			for _, name := range []string{"venv", ".venv"} {
				ownedTarget, err := pypath.Resolve(filepath.Join(d.ProjectRoot, name, "bin", "icebath"))
				if err != nil {
					return err
				}
				owned = owned || target == ownedTarget
			}
			if !owned {
				f.ManualIssues = append(f.ManualIssues, "Review "+display+"/icebath manually; its target is user-managed and was not changed")
				return nil
			}
			if !shouldFix {
				f.Issues = append(f.Issues, "Broken symlink at "+display+"/icebath — run 'icebath doctor --fix'")
				return nil
			}
			verb = "Fixed"
		case doctorPlatformPathExists(link):
			doctorCheckOK(w, display+"/icebath exists (non-symlink)", "")
			return nil
		default:
			doctorCheckFail(w, display+"/icebath not found", "(icebath command may not work outside the venv)")
			if !shouldFix {
				f.Issues = append(f.Issues, "Missing "+display+"/icebath symlink — run 'icebath doctor --fix'")
				return nil
			}
			if err := pyfs.Makedirs(linkDir, 0o777, true); err != nil {
				return err
			}
			verb = "Created"
		}
		if pmLauncher {
			staged, err := pm.StageLauncher(ctx, "icebath", d.ProjectRoot, linkDir)
			if err != nil {
				return err
			}
			if staged == "" {
				doctorCheckFail(w, "Could not publish Icebath launcher", "")
				f.ManualIssues = append(f.ManualIssues, "Repair the PM store interpreter through the installation owner, then rerun 'icebath doctor --fix'")
				return nil
			}
			doctorCheckOK(w, verb+" PM launcher: "+display+"/icebath", "")
		} else {
			if doctorIsSymlink(link) {
				if err := os.Remove(link); err != nil {
					return err
				}
			}
			if err := os.Symlink(venvBin, link); err != nil {
				return err
			}
			doctorCheckOK(w, verb+" symlink: "+display+"/icebath → "+venvBin, "")
		}
		f.Fixed++
		onPath := false
		for _, entry := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
			onPath = onPath || entry == linkDir
		}
		if verb == "Created" && !onPath {
			doctorCheckWarn(w, display+" is not on your PATH", "(add it to your shell config: export PATH=\"$HOME/.local/bin:$PATH\")")
			f.ManualIssues = append(f.ManualIssues, "Add "+display+" to your PATH")
		}
		return nil
	})
}
