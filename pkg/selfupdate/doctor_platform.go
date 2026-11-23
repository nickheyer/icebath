package selfupdate

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/install"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// doctorPackage is one library, program or runtime package an optional capability loads, no probe when its tier is not linked
type doctorPackage struct {
	Name    string
	Present func(ctx context.Context) bool
}

var (
	doctorDetectInstallMethod  = config.DetectInstallMethod
	doctorInstallTrustStore    = provider.InstallTrustStore
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
	doctorPackages             = []doctorPackage{
		{Name: "PortAudio (voice mode audio)"}, {Name: "Opus (Discord voice playback)"}, {Name: "FFmpeg (audio conversion)"},
		{Name: "Porcupine (wake word)"}, {Name: "sherpa-onnx (wake word)"}, {Name: "openWakeWord (wake word)"},
		{Name: "Python on PATH (execute_code)"},
	}
	// Matrix E2EE build state and the build command that adds it, nil in a build without the Matrix adapter
	doctorMatrixE2EE func() (builtIn bool, buildCommand string)
)

// Sets the probe of a capability row, called at init by the tier file that links the capability
func setDoctorPackageProbe(name string, present func(ctx context.Context) bool) {
	for i := range doctorPackages {
		if doctorPackages[i].Name == name {
			doctorPackages[i].Present = present
			return
		}
	}
	panic("doctor has no capability row " + name)
}

// Default client TLS context, the error its construction raises when a trust bundle cannot be loaded
var doctorDefaultTLSContext = func() error {
	cfg := provider.PlatformTLSConfig()
	if cfg.RootCAs == nil && cfg.VerifyConnection != nil {
		return cfg.VerifyConnection(tls.ConnectionState{})
	}
	return nil
}

// Reinstall instruction that respects who owns the installation, the setup script for a checkout
func doctorReinstallHint(ctx context.Context, d *Doctor) (string, error) {
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
	if method == install.MethodGit || install.IsSourceCheckout(d.ProjectRoot) {
		commands := install.CheckoutUpdateCommands()
		return "Run `" + commands[len(commands)-1] + "` in " + d.ProjectRoot + " to build and publish the icebath command", nil
	}
	return "Reinstall with `" + install.InstallerCommand() + "`", nil
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
		return errchain.OSText(err)
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
	up, err := mgr.IsRunning("main-icebath")
	if err != nil {
		return err
	}
	if up {
		doctorCheckOK(d.W, "main-icebath: up", "")
	} else {
		doctorCheckInfo(d.W, "main-icebath: down (expected if not enabled via env)")
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
	duplicates := []string{}
	for _, p := range slots {
		if p == "default" {
			continue
		}
		running, err := mgr.IsRunning("gateway-" + p)
		if err != nil {
			return err
		}
		if running {
			duplicates = append(duplicates, p)
		}
	}
	if len(duplicates) > 0 {
		doctorCheckWarn(w, "Per-profile gateway slots run beside the host gateway: "+strings.Join(duplicates, ", "),
			"(the host gateway already serves every profile from one process)")
		*issues = append(*issues, "Fold the per-profile gateways into the host gateway: "+
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
		doctorFailAndIssue(d.W, "TLS default SSL context cannot be constructed", err.Error(),
			"SSL_CERT_FILE ("+os.Getenv("SSL_CERT_FILE")+") holds no readable certificate: point it at a PEM CA bundle, or unset it to use the system trust store", issues)
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

// Reports compromised package advisories as manual issues with their remediation text
func checkSecurityAdvisories(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheckGuarded(d.W, "Security advisory check failed: {e}", "", func(f *Finding) error {
		allHits := plugins.DetectCompromised(ctx, plugins.Advisories)
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

// Row of the tool runtime the package manager staged, set by its tier and nil in a build without it
var doctorStagedRuntimeRow func(ctx context.Context, d *Doctor)

// Reports the Go runtime, the linked SQLite, the staged tool runtime, the macOS TCC anchor and Full Disk Access
func checkRuntime(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		doctorCheckOK(d.W, "Go "+hostinfo.GoVersion(), "")
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
		if doctorStagedRuntimeRow != nil {
			doctorStagedRuntimeRow(ctx, d)
		}
		if doctorTCCAnchorCheck != nil {
			if err := doctorTCCAnchorCheck(ctx, d, shouldFix); err != nil {
				return err
			}
		}
		return CheckMacosFullDiskAccess(d.W)
	})
}

// TCC anchor row of the managed interpreter, set by the package manager and nil in a build without it
var doctorTCCAnchorCheck func(ctx context.Context, d *Doctor, shouldFix bool) error

// Verifies the TLS policy, recording an unusable default context as a manual issue
func checkCertificates(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		return doctorCheckCertificatesRows(ctx, d, shouldFix, &f.ManualIssues)
	})
}

// Reports each linked optional capability's library or program and Matrix encryption, no section when the build links none
func checkOptionalCapabilities(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		titled := false
		section := func() {
			if !titled {
				doctorSection(d.W, "Optional Capabilities")
				titled = true
			}
		}
		for _, p := range doctorPackages {
			if p.Present == nil {
				continue
			}
			section()
			if p.Present(ctx) {
				doctorCheckOK(d.W, p.Name, "(optional)")
			} else {
				doctorCheckWarn(d.W, p.Name, "(optional, not installed)")
			}
		}
		if doctorMatrixE2EE != nil {
			section()
			if builtIn, command := doctorMatrixE2EE(); builtIn {
				doctorCheckOK(d.W, "Matrix E2EE (goolm)", "(optional)")
			} else {
				doctorCheckWarn(d.W, "Matrix E2EE (goolm)", "(optional, not built in: "+command+")")
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

// Checks the icebath command on PATH and the commands the installer publishes, republishing owned or missing ones with fix
func checkCommandInstallation(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		w := d.W
		doctorSection(w, "Command Installation")
		method, err := doctorDetectInstallMethod(ctx, d.Home, d.ProjectRoot)
		if err != nil {
			return err
		}
		mechanism, _ := pm.ReadInstallStamp(d.ProjectRoot).Get("updateMechanism")
		if config.IsNixInstallMethod(method) || method == "docker" || config.IsPackageManagerInstallMethod(method) || mechanism == "external" {
			if command := doctorWhich("icebath"); command != "" {
				doctorCheckOK(w, "Icebath command managed by "+method+" ("+command+")", "")
				return nil
			}
			doctorCheckWarn(w, "Icebath command not on PATH ("+method+"-managed)", "")
			hint, err := doctorReinstallHint(ctx, d)
			if err != nil {
				return err
			}
			f.ManualIssues = append(f.ManualIssues, hint)
			return nil
		}
		running := doctorRunningBinary()
		binary := pm.ResolveInstallBinary(d.ProjectRoot)
		switch {
		case binary == "" && install.IsSourceCheckout(d.ProjectRoot):
			doctorCheckInfo(w, "Source checkout "+d.ProjectRoot+" holds no built icebath binary, this process runs "+running)
		case binary == "":
			doctorCheckInfo(w, "This process runs "+running+", outside an install tree")
		case doctorSameFile(binary, running):
			doctorCheckOK(w, "Install binary "+binary, "(this process)")
		default:
			doctorCheckOK(w, "Install binary "+binary, "(this process runs "+running+")")
		}
		binDir, display, err := doctorUserBinDir()
		if err != nil {
			return err
		}
		if binary != "" {
			if err := doctorCheckPublishedCommands(ctx, d, binary, binDir, display, shouldFix, f); err != nil {
				return err
			}
		}
		return doctorCheckCommandOnPath(ctx, d, binary, running, binDir, display, f)
	})
}

// Binary this process runs with symlinks resolved
func doctorRunningBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

// Reports two paths naming one existing file
func doctorSameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// Reports a file a launcher publication writes, a shebang script or a cmd wrapper
func doctorIsWrapper(path string) bool {
	if strings.EqualFold(filepath.Ext(path), ".cmd") {
		return true
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	head := make([]byte, 2)
	n, _ := io.ReadFull(file, head)
	return n == 2 && string(head) == "#!"
}

// Binary a command finally runs through links and published wrappers, the first missing hop when the chain breaks
func doctorLaunchedBinary(command string) string {
	path := command
	for hops := 0; hops < 8; hops++ {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return path
		}
		if !doctorIsWrapper(resolved) {
			return resolved
		}
		next := pm.LauncherBinary(resolved)
		if next == "" || next == resolved {
			return resolved
		}
		path = next
	}
	return path
}

// User bin the installer publishes commands into and its display form, the home bin on Windows
func doctorUserBinDir() (string, string, error) {
	if doctorSysPlatform == "windows" {
		dir := filepath.Join(home.Root(), "bin")
		return dir, dir, nil
	}
	userHome, err := doctorUserHomeDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(userHome, ".local", "bin"), "~/.local/bin", nil
}

// Command file the installer writes in the user bin for one command name
func doctorPublishedCommandFile(name string) string {
	if doctorSysPlatform == "windows" {
		return name + ".cmd"
	}
	return name
}

// Rows of the icebath and icebath-acp commands in the user bin, republished with fix when missing or pointing elsewhere
func doctorCheckPublishedCommands(ctx context.Context, d *Doctor, binary, binDir, display string, shouldFix bool, f *Finding) error {
	w := d.W
	disabled, err := pm.ExposeOnPathDisabled(ctx)
	if err != nil {
		return err
	}
	if disabled {
		doctorCheckInfo(w, "cli.expose_on_path is false, no command is published in "+display)
		return nil
	}
	repair := []string{}
	for _, name := range pm.WindowsBinLaunchers() {
		file := doctorPublishedCommandFile(name)
		path, shown := filepath.Join(binDir, file), display+"/"+file
		if doctorSysPlatform == "windows" {
			shown = path
		}
		if _, err := os.Lstat(path); err != nil {
			doctorCheckFail(w, shown+" not found", "")
			repair = append(repair, shown)
			continue
		}
		runs := doctorLaunchedBinary(path)
		switch {
		case doctorSameFile(runs, binary):
			doctorCheckOK(w, shown+" → "+binary, "")
		case pm.OwnsLauncher(path, d.ProjectRoot):
			detail := "(→ " + runs + ", expected → " + binary + ")"
			if !doctorPlatformPathExists(runs) {
				detail = "(→ " + runs + ", which does not exist)"
			}
			doctorCheckWarn(w, shown+" points to wrong target", detail)
			repair = append(repair, shown)
		default:
			doctorCheckWarn(w, shown+" runs "+runs, "(user-managed, not changed)")
			f.ManualIssues = append(f.ManualIssues, "Review "+shown+" manually; it runs "+runs+" instead of "+binary)
		}
	}
	if len(repair) == 0 {
		return nil
	}
	if !shouldFix {
		f.Issues = append(f.Issues, "Publish "+strings.Join(repair, ", ")+" — run 'icebath doctor --fix'")
		return nil
	}
	result, err := pm.ExposeCLI(ctx, d.ProjectRoot, true)
	if err != nil {
		return err
	}
	switch {
	case !result.OK:
		doctorCheckFail(w, "Could not publish "+strings.Join(repair, ", "), "("+result.Error+")")
		hint, err := doctorReinstallHint(ctx, d)
		if err != nil {
			return err
		}
		f.ManualIssues = append(f.ManualIssues, hint)
	case result.Skipped != "":
		doctorCheckWarn(w, "Commands not published in "+display, "("+result.Skipped+")")
		f.ManualIssues = append(f.ManualIssues, "Publish "+strings.Join(repair, ", ")+" from the install that owns "+display)
	default:
		doctorCheckOK(w, "Published "+strings.Join(repair, ", "), "")
		f.Fixed++
	}
	return nil
}

// Row of the icebath command a shell finds on PATH and the binary it runs
func doctorCheckCommandOnPath(ctx context.Context, d *Doctor, binary, running, binDir, display string, f *Finding) error {
	w := d.W
	command := doctorWhich("icebath")
	if command == "" {
		doctorCheckWarn(w, "icebath is not on PATH", "")
		_, err := os.Lstat(filepath.Join(binDir, doctorPublishedCommandFile("icebath")))
		switch {
		case binary == "":
			hint, err := doctorReinstallHint(ctx, d)
			if err != nil {
				return err
			}
			f.ManualIssues = append(f.ManualIssues, hint)
		case err != nil:
		case doctorSysPlatform == "windows":
			f.ManualIssues = append(f.ManualIssues, "Add "+binDir+" to your User PATH, then open a new console")
		default:
			f.ManualIssues = append(f.ManualIssues, "Add "+display+" to your PATH (export PATH=\"$HOME/.local/bin:$PATH\" in your shell config)")
		}
		return nil
	}
	want := binary
	if want == "" {
		want = running
	}
	runs := doctorLaunchedBinary(command)
	switch {
	case doctorSameFile(runs, want):
		doctorCheckOK(w, "icebath on PATH runs "+runs, "("+command+")")
	case binary == "":
		doctorCheckInfo(w, "icebath on PATH ("+command+") runs "+runs+", not this process")
	default:
		doctorCheckWarn(w, "icebath on PATH runs another binary", "("+command+" → "+runs+", expected "+binary+")")
		if !doctorSameFile(filepath.Dir(command), binDir) {
			f.ManualIssues = append(f.ManualIssues, "Put "+display+" ahead of "+filepath.Dir(command)+" on PATH, or remove "+command)
		}
	}
	return nil
}
