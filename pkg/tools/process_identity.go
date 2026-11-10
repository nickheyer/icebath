package tools

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Spawn tags, the machine wide spawn ledger, the Windows job object and the POSIX owner death supervisor

// SpawnEnvVar carries the spawn tag a spawner stamps on every long lived child
const SpawnEnvVar = "ICEBATH_SPAWN"

const spawnTagVersion = "v1"

// LedgerFilename is the spawn ledger file under the root home
const LedgerFilename = "spawn-ledger.json"

// ReapablePurposes are the purposes a reaper may kill once their owner is gone
var ReapablePurposes = setOf("serve", "dashboard", "gateway", "mcp-helper")

var backendOwnerPurposes = setOf("serve", "dashboard", "gateway")

var errNoSuchProcess = proc.ErrNoSuchProcess

// Process facts the identity layer reads, swapped by tests
type processProber interface {
	Birth(pid int) (float64, error)
	WorkerBirth(pid int) (float64, error)
	BootStart(pid int) (float64, bool)
	Running(pid int) bool
	Zombie(pid int) bool
	Environ(pid int) (map[string]string, error)
	Cmdline(pid int) ([]string, error)
	PPID(pid int) (int, bool)
	Children(pid int, recursive bool) []int
	Terminate(pid int) error
	Kill(pid int) error
}

var identityProber processProber = osProcessProber{}

var identityLogger = func() *slog.Logger { return slog.Default() }

// Pid of this process, swapped by tests that stamp another spawner
var currentPid = os.Getpid

// Ledger read the reapers consult, swapped by tests
var ledgerEntriesFn = func(opts LedgerOptions) []map[string]any { return LedgerEntries(opts) }

// Desktop SSH lock claims the reparented orphan rung consults, swapped by tests
var lockOwnedServePidsFn = func() map[int]bool { return lockOwnedServePids("") }

// Group existence probe the supervisor bookkeeping uses, swapped by tests
var processGroupGoneFn = func(pgid int) bool { return processGroupGone(pgid) }

// RegisterChildFn records an owned child in the spawn ledger, swappable so a test observes the record
var RegisterChildFn = func(ctx context.Context, pid int, purpose, projectRoot string) bool {
	return RegisterChild(ctx, pid, purpose, projectRoot)
}

// OwnProcessGroupsFn makes process groups die with this process, swappable so a test observes the call
var OwnProcessGroupsFn = func(pgids []int) { ownProcessGroups(pgids) }

// ReleaseProcessGroupsFn stops covering process groups this process stopped, swappable so a test observes the call
var ReleaseProcessGroupsFn = func(pgids []int) { releaseProcessGroups(pgids) }

// Executable directory of this install, the project root a spawner identifies itself by
var installRootFn = func() string {
	exe, err := os.Executable()
	if err != nil {
		return RunningSourceRoot()
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

// InstallID is the stable 12 hex identifier of this install derived from its path
func InstallID(projectRoot string) string {
	if projectRoot == "" {
		projectRoot = installRootFn()
	}
	canonical := projectRoot
	if abs, err := filepath.Abs(projectRoot); err == nil {
		canonical = abs
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			canonical = resolved
		}
	}
	sum := sha256.Sum256([]byte(strings.ToLower(canonical)))
	return hex.EncodeToString(sum[:])[:12]
}

// ProcessBirth is the epoch birth of a managed worker, the same in every process
func ProcessBirth(pid int) (float64, error) { return identityProber.Birth(pid) }

// WorkerBirth is the birth the identity fences compare, boot ticks on Linux and ProcessBirth elsewhere
func WorkerBirth(pid int) (float64, error) { return identityProber.WorkerBirth(pid) }

// BootStart is the seconds from boot to the start of pid on Linux, false elsewhere
func BootStart(pid int) (float64, bool) { return identityProber.BootStart(pid) }

// ProcessBirth of pid or of this process when pid is zero, nil when unreadable
func processCreateTime(pid int) *float64 {
	if pid <= 0 {
		pid = currentPid()
	}
	t, err := identityProber.Birth(pid)
	if err != nil {
		return nil
	}
	return &t
}

// ProcessBirth of pid or of this process when pid is zero, nil when unreadable
func ProcessCreateTime(pid int) *float64 { return processCreateTime(pid) }

var liveBootIDOnce = sync.OnceValue(readLiveBootID)

// Kernel id of the running boot on Linux, empty elsewhere or when unreadable
var liveBootID = func() string { return liveBootIDOnce() }

func readLiveBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// No live process has a negative boot start, so an entry from an earlier boot never matches
const earlierBoot = -1.0

// Boot relative birth of the entry as exact proof, nil when either boot id is unknown
func entryBoot(entry map[string]any, key string) *float64 {
	recorded, ok := entry["boot_id"].(string)
	live := liveBootID()
	if !ok || live == "" {
		return nil
	}
	if recorded == live {
		return entryFloat(entry, key)
	}
	v := earlierBoot
	return &v
}

// BootStart of pid while it still matches createTime, nil when unproven or the boot is unidentifiable
func verifiedBootStart(pid int, createTime *float64) *float64 {
	if pid <= 0 || createTime == nil || liveBootID() == "" {
		return nil
	}
	if !sameIncarnation(pid, createTime, nil) {
		return nil
	}
	if !identityProber.Running(pid) {
		return nil
	}
	v, ok := identityProber.BootStart(pid)
	if !ok {
		return nil
	}
	return &v
}

// SpawnTag is the parsed value of the spawn env var
type SpawnTag struct {
	Install       string
	Purpose       string
	SpawnerPid    int
	SpawnerCreate *float64
}

// BuildSpawnTag is the value for the child's spawn env var, stamped by the spawner
func BuildSpawnTag(purpose, projectRoot string) string {
	createPart := "-"
	if create := processCreateTime(0); create != nil {
		createPart = strconv.FormatFloat(*create, 'f', 3, 64)
	}
	return strings.Join([]string{spawnTagVersion, InstallID(projectRoot), purpose, strconv.Itoa(currentPid()), createPart}, ":")
}

// SpawnEnv is the env fragment a spawner merges into a child's environment
func SpawnEnv(purpose, projectRoot string) map[string]string {
	return map[string]string{SpawnEnvVar: BuildSpawnTag(purpose, projectRoot)}
}

// ParseSpawnTag parses a spawn env value, nil for anything malformed
func ParseSpawnTag(raw any) *SpawnTag {
	text, ok := raw.(string)
	if !ok {
		return nil
	}
	parts := strings.Split(text, ":")
	if len(parts) != 5 || parts[0] != spawnTagVersion {
		return nil
	}
	install, purpose, pidS, createS := parts[1], parts[2], parts[3], parts[4]
	if install == "" || purpose == "" {
		return nil
	}
	pid, err := strconv.Atoi(pidS)
	if err != nil {
		return nil
	}
	var create *float64
	if createS != "-" {
		v, err := strconv.ParseFloat(createS, 64)
		if err != nil {
			return nil
		}
		create = &v
	}
	if pid <= 0 {
		return nil
	}
	return &SpawnTag{Install: install, Purpose: purpose, SpawnerPid: pid, SpawnerCreate: create}
}

// SpawnedByThisProcess reports whether pid carries a spawn tag naming this process by pid and create time
func SpawnedByThisProcess(pid int) bool {
	environ, err := identityProber.Environ(pid)
	if err != nil {
		return false
	}
	tag := ParseSpawnTag(environ[SpawnEnvVar])
	if tag == nil || tag.SpawnerPid != currentPid() {
		return false
	}
	mine := processCreateTime(0)
	return tag.SpawnerCreate == nil || mine == nil || math.Abs(*tag.SpawnerCreate-*mine) < 2.0
}

// LedgerEntry is one spawn ledger row
type LedgerEntry struct {
	Pid           int      `json:"pid"`
	CreateTime    *float64 `json:"create_time"`
	Purpose       string   `json:"purpose"`
	Install       string   `json:"install"`
	SpawnerPid    *int     `json:"spawner_pid"`
	SpawnerCreate *float64 `json:"spawner_create"`
	RegisteredAt  float64  `json:"registered_at"`
	Argv          string   `json:"argv"`
	Host          string   `json:"host"`
	Port          *int     `json:"port"`
	Profile       string   `json:"profile"`
	IcebathHome   string   `json:"icebath_home"`
	Isolated      bool     `json:"isolated"`
	BootStart     *float64 `json:"boot_start"`
	SpawnerBoot   *float64 `json:"spawner_boot"`
	BootID        *string  `json:"boot_id"`
}

// Row of the entry in the ledger's JSON shape and dataclass field order, built without a JSON round trip
func (e LedgerEntry) asObject() *pyjson.Object {
	o := pyjson.ObjectOf(
		"pid", e.Pid, "create_time", nil, "purpose", e.Purpose, "install", e.Install, "spawner_pid", nil,
		"spawner_create", nil, "registered_at", e.RegisteredAt, "argv", e.Argv, "host", e.Host, "port", nil,
		"profile", e.Profile, "icebath_home", e.IcebathHome, "isolated", e.Isolated, "boot_start", nil,
		"spawner_boot", nil, "boot_id", nil)
	if e.CreateTime != nil {
		o.Set("create_time", *e.CreateTime)
	}
	if e.SpawnerPid != nil {
		o.Set("spawner_pid", *e.SpawnerPid)
	}
	if e.SpawnerCreate != nil {
		o.Set("spawner_create", *e.SpawnerCreate)
	}
	if e.Port != nil {
		o.Set("port", *e.Port)
	}
	if e.BootStart != nil {
		o.Set("boot_start", *e.BootStart)
	}
	if e.SpawnerBoot != nil {
		o.Set("spawner_boot", *e.SpawnerBoot)
	}
	if e.BootID != nil {
		o.Set("boot_id", *e.BootID)
	}
	return o
}

// Machine root ledger path shared by every profile of this install
var ledgerPathFn = func() string {
	return filepath.Join(home.Root(), LedgerFilename)
}

// Integer field of a ledger row, false for a missing, fractional or non numeric value
func entryInt(entry map[string]any, key string) (int, bool) {
	switch v := entry[key].(type) {
	case json.Number:
		n, err := strconv.ParseInt(v.String(), 10, 64)
		if err != nil {
			return 0, false
		}
		return int(n), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Float field of a ledger row, nil when missing or non numeric
func entryFloat(entry map[string]any, key string) *float64 {
	switch v := entry[key].(type) {
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return nil
		}
		return &f
	case float64:
		return &v
	case int:
		f := float64(v)
		return &f
	case int64:
		f := float64(v)
		return &f
	}
	return nil
}

// Entries list, empty for a missing or blank file, nil for a corrupt one
func readLedger(path string) ([]*pyjson.Object, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []*pyjson.Object{}, true
		}
		return nil, false
	}
	text := strings.TrimPrefix(string(data), "\xEF\xBB\xBF")
	if strings.TrimSpace(text) == "" {
		return []*pyjson.Object{}, true
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return nil, false
	}
	list, ok := parsed.([]any)
	if !ok {
		return nil, false
	}
	out := make([]*pyjson.Object, 0, len(list))
	for _, item := range list {
		if m, ok := item.(*pyjson.Object); ok {
			out = append(out, m)
		}
	}
	return out, true
}

// Ledger entries, false after parking a corrupt file, caller holds the ledger lock
func readLedgerOrQuarantine(path string) ([]*pyjson.Object, bool) {
	entries, ok := readLedger(path)
	if !ok {
		parked := path + ".corrupt"
		if err := os.Rename(path, parked); err == nil {
			identityLogger().Warn(fmt.Sprintf("spawn ledger was unreadable; moved to %s", parked))
		}
	}
	return entries, ok
}

// Reports whether the live pid matches a recorded birth, exact boot ticks first then the 2s epoch window
func sameIncarnation(pid int, createTime *float64, boot *float64) bool {
	if boot != nil {
		if live, ok := identityProber.BootStart(pid); ok {
			return live == *boot
		}
	}
	if createTime == nil {
		return true
	}
	birth, err := identityProber.Birth(pid)
	if err != nil {
		return false
	}
	return math.Abs(birth-*createTime) < 2.0
}

// Tri state liveness proof of pid against a recorded birth, known false when the prober cannot say
func pidAliveMatches(pid int, createTime *float64, strict bool, boot *float64) (alive bool, known bool) {
	if !identityProber.Running(pid) {
		return false, true
	}
	birth, err := identityProber.Birth(pid)
	if err != nil {
		if errors.Is(err, errNoSuchProcess) {
			return false, true
		}
		return false, false
	}
	if strict {
		if isWindowsHost {
			return createTime != nil && birth == *createTime, true
		}
		return createTime != nil && birth == *createTime && !identityProber.Zombie(pid), true
	}
	return sameIncarnation(pid, createTime, boot) && (isWindowsHost || !identityProber.Zombie(pid)), true
}

// Tri state liveness proof of pid against a recorded birth, known false when the prober cannot say
func PidAliveMatches(pid int, createTime *float64, strict bool, boot *float64) (alive bool, known bool) {
	return pidAliveMatches(pid, createTime, strict, boot)
}

// RegisterSelf records this process in the machine spawn ledger
func RegisterSelf(ctx context.Context, purpose, projectRoot string, detail map[string]any) bool {
	tag := ParseSpawnTag(os.Getenv(SpawnEnvVar))
	var spawnerPid *int
	var spawnerCreate *float64
	if tag != nil {
		pid := tag.SpawnerPid
		spawnerPid, spawnerCreate = &pid, tag.SpawnerCreate
	} else {
		spawnerPid, spawnerCreate = desktopSpawnerIdentity()
	}
	entry := newLedgerEntry(currentPid(), processCreateTime(0), purpose, projectRoot, spawnerPid, spawnerCreate)
	if spawnerPid != nil {
		entry.SpawnerBoot = verifiedBootStart(*spawnerPid, spawnerCreate)
	}
	entry.IcebathHome = home.Key(home.Process())
	if detail != nil {
		if host, ok := detail["host"]; ok && host != nil {
			entry.Host = fmt.Sprint(host)
		}
		if port, ok := detail["port"]; ok && port != nil {
			if n, ok := asIntValue(port); ok {
				entry.Port = &n
			}
		}
		if profile, ok := detail["profile"]; ok && profile != nil {
			entry.Profile = fmt.Sprint(profile)
		}
		if isolated, ok := detail["isolated"].(bool); ok {
			entry.Isolated = isolated
		}
	}
	argv := os.Args
	if len(argv) > 10 {
		argv = argv[:10]
	}
	entry.Argv = strings.Join(argv, " ")
	return appendLedgerEntry(ctx, entry)
}

// IsDesktopOwnedBackend reports whether this process is the backend the Desktop spawned and owns
func IsDesktopOwnedBackend(argv []string) bool {
	if os.Getenv("ICEBATH_DESKTOP") != "1" {
		return false
	}
	if os.Getenv("ICEBATH_DASHBOARD_SESSION_TOKEN") != "" {
		return true
	}
	if argv == nil {
		argv = os.Args[1:]
	}
	return IsDesktopSSHBackendArgv(argv)
}

// IsDesktopSSHBackendArgv reports whether argv is the Desktop client's SSH backend spawn
func IsDesktopSSHBackendArgv(argv []string) bool {
	for _, a := range argv {
		if a == "--ssh-session-token-file" {
			return true
		}
	}
	return false
}

type servingLocalDesktopKey struct{}

// WithServingLocalDesktop marks the enclosed activity as work for the local Desktop
func WithServingLocalDesktop(ctx context.Context, active bool) context.Context {
	return context.WithValue(ctx, servingLocalDesktopKey{}, active || ServesLocalDesktopNow(ctx))
}

// ServesLocalDesktopNow reports whether the activity serves the Desktop app on this machine
func ServesLocalDesktopNow(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(servingLocalDesktopKey{}).(bool)
	return v
}

// LocalDesktopClient reports a client that declares itself local Desktop and redeemed a native ticket
func LocalDesktopClient(identity map[string]any, declared any) bool {
	if declared != true || identity == nil {
		return false
	}
	return identity["provider"] == "local" && identity["native_bootstrap"] == true
}

// Spawner pid and create time from the Electron parent death watchdog env vars
func desktopSpawnerIdentity() (*int, *float64) {
	spawnerPid, err := strconv.Atoi(os.Getenv("ICEBATH_PARENT_PID"))
	if err != nil || spawnerPid <= 0 {
		return nil, nil
	}
	marker := os.Getenv("ICEBATH_PARENT_START_MARKER")
	if !strings.HasPrefix(marker, "winms:") {
		return &spawnerPid, nil
	}
	ms, err := strconv.ParseFloat(strings.SplitN(marker, ":", 2)[1], 64)
	if err != nil {
		return &spawnerPid, nil
	}
	create := ms / 1000.0
	return &spawnerPid, &create
}

func newLedgerEntry(pid int, createTime *float64, purpose, projectRoot string, spawnerPid *int, spawnerCreate *float64) LedgerEntry {
	entry := LedgerEntry{
		Pid: pid, CreateTime: createTime, Purpose: purpose, Install: InstallID(projectRoot),
		SpawnerPid: spawnerPid, SpawnerCreate: spawnerCreate,
		RegisteredAt: float64(time.Now().UnixNano()) / 1e9,
		BootStart:    verifiedBootStart(pid, createTime),
	}
	if id := liveBootID(); id != "" {
		entry.BootID = &id
	}
	return entry
}

// Past this a stalled ledger writer no longer stalls every spawn on the machine
var ledgerLockTimeout = 2 * time.Second

var (
	ledgerMu         sync.Mutex
	ledgerLockWarned bool
)

// Prunes dead entries and appends entry, the only ledger write path
func appendLedgerEntry(ctx context.Context, entry LedgerEntry) bool {
	path := ledgerPathFn()
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	lockPath := path + ".lock"
	os.MkdirAll(filepath.Dir(lockPath), 0o700)
	unlock, err := lockStoreFile(lockPath, ledgerLockTimeout)
	if err != nil {
		if !ledgerLockWarned {
			ledgerLockWarned = true
			identityLogger().Warn("spawn ledger lock unavailable (a stalled holder, or no file locks here); writing without it, so concurrent icebath processes may drop each other's rows")
		}
		identityLogger().Debug("spawn ledger lock unavailable", "error", err)
	} else {
		defer unlock()
	}
	entries, _ := readLedgerOrQuarantine(path)
	pruned := make([]*pyjson.Object, 0, len(entries)+1)
	for _, e := range entries {
		pid, ok := entryInt(e.Vals, "pid")
		if !ok || pid == entry.Pid || !EntryStillRunning(e.Vals) {
			continue
		}
		pruned = append(pruned, e)
	}
	pruned = append(pruned, entry.asObject())
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		identityLogger().Debug("spawn ledger write failed", "error", err)
		return false
	}
	if err := procAtomicJSONWrite(path, pruned, 0o600, true); err != nil {
		identityLogger().Debug("spawn ledger write failed", "error", err)
		return false
	}
	return true
}

// Writes data as indented JSON through a private temp file and an atomic rename
func procAtomicJSONWrite(path string, data any, mode os.FileMode, ensureASCII bool) error {
	text, err := pyjson.Dumps(data, pyjson.Indent(2), pyjson.EnsureASCII(ensureASCII))
	if err == nil && !ensureASCII && !utf8.ValidString(text) {
		text, err = pyjson.Dumps(data, pyjson.Indent(2), pyjson.EnsureASCII(true))
	}
	if err != nil {
		return err
	}
	body := []byte(text)
	dir := filepath.Dir(path)
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	tmp, err := os.CreateTemp(dir, "."+stem+"_*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }
	if err := tmp.Chmod(mode); err != nil && !isWindowsHost {
		cleanup()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Writes a JSON string with every non ASCII rune escaped and invalid bytes kept as surrogate escapes
func writeASCIIJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(b, "\\u%04x", 0xDC00+int(s[i]))
			i++
			continue
		}
		i += size
		switch {
		case r == '"':
			b.WriteString("\\\"")
		case r == '\\':
			b.WriteString("\\\\")
		case r == '\n':
			b.WriteString("\\n")
		case r == '\r':
			b.WriteString("\\r")
		case r == '\t':
			b.WriteString("\\t")
		case r == '\b':
			b.WriteString("\\b")
		case r == '\f':
			b.WriteString("\\f")
		case r < 0x20 || (r >= 0x7f && r <= 0xFFFF):
			fmt.Fprintf(b, "\\u%04x", r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(b, "\\u%04x\\u%04x", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// RegisterChild records a child this process just spawned with this process as its spawner
func RegisterChild(ctx context.Context, pid int, purpose, projectRoot string) bool {
	if pid <= 0 {
		return false
	}
	childCreate := processCreateTime(pid)
	if childCreate == nil {
		return false
	}
	self := currentPid()
	entry := newLedgerEntry(pid, childCreate, purpose, projectRoot, &self, processCreateTime(0))
	entry.SpawnerBoot = verifiedBootStart(self, entry.SpawnerCreate)
	if argv, err := identityProber.Cmdline(pid); err == nil {
		if len(argv) > 10 {
			argv = argv[:10]
		}
		entry.Argv = strings.Join(argv, " ")
	}
	return appendLedgerEntry(ctx, entry)
}

// LedgerOptions narrows a ledger read
type LedgerOptions struct {
	ProjectRoot  string
	AllInstalls  bool
	VerifiedOnly bool
}

// LedgerEntries lists the live rows of this install, or of every install when asked
func LedgerEntries(opts LedgerOptions) []map[string]any {
	wantInstall := InstallID(opts.ProjectRoot)
	ledgerMu.Lock()
	entries, ok := readLedgerOrQuarantine(ledgerPathFn())
	ledgerMu.Unlock()
	if !ok {
		return []map[string]any{}
	}
	out := []map[string]any{}
	for _, row := range entries {
		e := row.Vals
		if !opts.AllInstalls && e["install"] != wantInstall {
			continue
		}
		pid, isInt := entryInt(e, "pid")
		if !isInt {
			continue
		}
		if opts.VerifiedOnly {
			alive, known := pidAliveMatches(pid, entryFloat(e, "create_time"), true, nil)
			if !(known && alive) {
				continue
			}
		} else {
			alive, known := pidAliveMatches(pid, entryFloat(e, "create_time"), false, entryBoot(e, "boot_start"))
			if known && !alive {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// EntryStillRunning reports whether the entry's process is still the recorded incarnation
func EntryStillRunning(entry map[string]any) bool {
	pid, ok := entryInt(entry, "pid")
	if !ok || pid <= 0 {
		return false
	}
	alive, known := pidAliveMatches(pid, entryFloat(entry, "create_time"), false, entryBoot(entry, "boot_start"))
	return !(known && !alive)
}

// SpawnerIsDead reports whether the recorded spawner is provably gone, known false when unprovable
func SpawnerIsDead(entry map[string]any) (dead bool, known bool) {
	spawnerPid, ok := entryInt(entry, "spawner_pid")
	if !ok || spawnerPid <= 0 {
		return false, false
	}
	alive, known := pidAliveMatches(spawnerPid, entryFloat(entry, "spawner_create"), false, entryBoot(entry, "spawner_boot"))
	if !known {
		return false, false
	}
	return !alive, true
}

// SIGTERM, 2s grace, then SIGKILL, false when the incarnation moved on or is already gone
func terminateThenKill(pid int, createTime *float64, boot *float64) bool {
	if !identityProber.Running(pid) {
		return false
	}
	if !sameIncarnation(pid, createTime, boot) {
		return false
	}
	if err := identityProber.Terminate(pid); err != nil {
		identityLogger().Debug(fmt.Sprintf("orphan terminate failed for pid %d", pid), "error", err)
		return false
	}
	if !waitProcessGone(pid, 2*time.Second) {
		if err := identityProber.Kill(pid); err != nil {
			identityLogger().Debug(fmt.Sprintf("orphan terminate failed for pid %d", pid), "error", err)
			return false
		}
	}
	return true
}

// Polls until pid is gone or a zombie, false at the timeout
func waitProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !identityProber.Running(pid) || identityProber.Zombie(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Covers the gap between process start and the Desktop client writing its lock file
const reapMinAgeSeconds = 180.0

// Null spawner owner reparented to init, old and claimed by no Desktop SSH lock
func reparentedOrphan(pid int) bool {
	ppid, ok := identityProber.PPID(pid)
	if !ok || (ppid != 0 && ppid != 1) {
		return false
	}
	if lockOwnedServePidsFn()[pid] {
		return false
	}
	birth, err := identityProber.Birth(pid)
	if err != nil {
		return false
	}
	return math.Max(0, float64(time.Now().UnixNano())/1e9-birth) >= reapMinAgeSeconds
}

const (
	backendLockSchemaVersion = 2
	backendLockProtocol      = 1
	remoteLockSubdir         = "desktop-ssh"
	// BackendLockName is the Desktop SSH runtime's per backend lock file
	BackendLockName = "backend.lock.json"
)

func isLowerHex(value any, length int) bool {
	s, ok := value.(string)
	if !ok || len(s) != length {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// Validates a parsed backend lock body against the Desktop writer's schema
func validLockfilePayload(parsed any, ownershipID string) bool {
	obj, ok := parsed.(*pyjson.Object)
	if !ok {
		return false
	}
	m := obj.Vals
	if !pyjson.Equal(m["schemaVersion"], int64(backendLockSchemaVersion)) || !pyjson.Equal(m["protocolVersion"], int64(backendLockProtocol)) || m["ownershipId"] != ownershipID {
		return false
	}
	if !isLowerHex(m["spawnNonce"], 16) || !isLowerHex(m["tokenFingerprint"], 32) {
		return false
	}
	pid, okPid := entryInt(m, "pid")
	port, okPort := entryInt(m, "port")
	if !okPid || !okPort || pid <= 0 || pid > 4194304 || port < 0 || port > 65535 {
		return false
	}
	for _, f := range []string{"profile", "icebathPath", "icebathHome", "logPath", "startedAt"} {
		s, ok := m[f].(string)
		if !ok {
			alias := strings.Replace(f, "icebath", "hermes", 1)
			if s, ok = m[alias].(string); !ok {
				return false
			}
		}
		if pystr.Len(s) > 1024 {
			return false
		}
	}
	logPath, _ := m["logPath"].(string)
	return strings.HasSuffix(logPath, fmt.Sprintf("/%s/%s.log", ownershipID, m["spawnNonce"]))
}

// ReadValidBackendLock returns the validated body of one Desktop SSH backend lock, nil otherwise
func ReadValidBackendLock(lockPath string) map[string]any {
	ownershipID := filepath.Base(filepath.Dir(lockPath))
	if !isLowerHex(ownershipID, 32) {
		return nil
	}
	st, err := os.Stat(lockPath)
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(lockPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, 65537)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil
	}
	if n > 65536 {
		return nil
	}
	parsed, err := pyjson.LoadsBytes(buf[:n])
	if err != nil {
		return nil
	}
	if !validLockfilePayload(parsed, ownershipID) {
		return nil
	}
	return parsed.(*pyjson.Object).Vals
}

// Every directory the Desktop may have written an SSH backend lock under
func remoteLockRoots(baseDir string) []string {
	if baseDir != "" {
		return []string{baseDir}
	}
	roots := []string{}
	for _, h := range []string{home.Process(), home.Root()} {
		root := filepath.Join(h, remoteLockSubdir)
		if !containsString(roots, root) {
			roots = append(roots, root)
		}
	}
	return roots
}

// Pids claimed by valid Desktop SSH backend lock records
func lockOwnedServePids(baseDir string) map[int]bool {
	owned := map[int]bool{}
	for _, root := range remoteLockRoots(baseDir) {
		st, err := os.Stat(root)
		if err != nil || !st.IsDir() {
			continue
		}
		ents, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, ent := range ents {
			lock := ReadValidBackendLock(filepath.Join(root, ent.Name(), BackendLockName))
			if lock != nil {
				pid, _ := entryInt(lock, "pid")
				owned[pid] = true
			}
		}
	}
	return owned
}

// ReapOrphanedBackendOwner kills a conflicting host owner the ledger proves is a dead session's orphan
func ReapOrphanedBackendOwner(pid int, createTime *float64, killFn func(int) error) (reaped int, ok bool) {
	if pid <= 0 || pid == currentPid() {
		return 0, false
	}
	defer func() {
		if recover() != nil {
			reaped, ok = 0, false
		}
	}()
	var entry map[string]any
	for _, e := range ledgerEntriesFn(LedgerOptions{}) {
		epid, ok := entryInt(e, "pid")
		purpose, _ := e["purpose"].(string)
		if !ok || epid != pid || !backendOwnerPurposes[purpose] {
			continue
		}
		ec := entryFloat(e, "create_time")
		if ec != nil && createTime != nil && math.Abs(*ec-*createTime) >= 2.0 {
			continue
		}
		entry = e
		break
	}
	if entry == nil {
		return 0, false
	}
	dead, known := SpawnerIsDead(entry)
	if !(known && dead) && (entry["spawner_pid"] != nil || !reparentedOrphan(pid)) {
		return 0, false
	}
	if killFn != nil {
		if err := killFn(pid); err != nil {
			identityLogger().Debug(fmt.Sprintf("backend owner reap failed for %v", entry), "error", err)
			return 0, false
		}
	} else if !terminateThenKill(pid, entryFloat(entry, "create_time"), entryBoot(entry, "boot_start")) {
		return 0, false
	}
	identityLogger().Info(fmt.Sprintf("reaped orphaned %v backend owner pid %d", entry["purpose"], pid))
	return pid, true
}

// ReapOrphanedMCPHelpers kills ledger registered stdio MCP helpers whose spawner is provably dead
func ReapOrphanedMCPHelpers(projectRoot string, killFn func(int) error) []int {
	reaped := []int{}
	var entries []map[string]any
	func() {
		defer func() { recover() }()
		entries = ledgerEntriesFn(LedgerOptions{ProjectRoot: projectRoot})
	}()
	own := currentPid()
	for _, entry := range entries {
		pid, ok := entryInt(entry, "pid")
		if entry["purpose"] != "mcp-helper" || !ok || pid <= 0 || pid == own {
			continue
		}
		dead, known := SpawnerIsDead(entry)
		if !(known && dead) {
			continue
		}
		if killFn != nil {
			if err := killFn(pid); err != nil {
				identityLogger().Debug(fmt.Sprintf("mcp-helper orphan reap failed for %v", entry), "error", err)
				continue
			}
		} else {
			if !identityProber.Running(pid) || !sameIncarnation(pid, entryFloat(entry, "create_time"), entryBoot(entry, "boot_start")) {
				continue
			}
			if isWindowsHost {
				killProcessTreeWindows(pid)
			} else {
				if err := identityProber.Terminate(pid); err != nil {
					identityLogger().Debug(fmt.Sprintf("mcp-helper orphan reap failed for %v", entry), "error", err)
					continue
				}
				if !waitProcessGone(pid, 2*time.Second) {
					identityProber.Kill(pid)
				}
			}
		}
		reaped = append(reaped, pid)
	}
	if len(reaped) > 0 {
		identityLogger().Info(fmt.Sprintf("reaped %d orphaned stdio MCP helper(s): %v", len(reaped), reaped))
	}
	return reaped
}

// Terminates pid and every still alive descendant, then kills the survivors after 2s
func killProcessTreeWindows(pid int) {
	descendants := identityProber.Children(pid, true)
	for _, child := range descendants {
		identityProber.Terminate(child)
	}
	identityProber.Terminate(pid)
	all := append(append([]int{}, descendants...), pid)
	deadline := time.Now().Add(2 * time.Second)
	for {
		alive := all[:0]
		for _, p := range all {
			if identityProber.Running(p) {
				alive = append(alive, p)
			}
		}
		all = alive
		if len(all) == 0 {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, survivor := range all {
		identityProber.Kill(survivor)
	}
}

// AttachSelfToKillOnCloseJob places this process in a job that dies whole when it dies, Windows only
func AttachSelfToKillOnCloseJob() bool { return attachSelfToKillOnCloseJob() }

//go:embed process_supervisor.sh
var supervisorScriptFS embed.FS

// Script text of the owner death supervisor
var supervisorScript = func() string {
	data, err := supervisorScriptFS.ReadFile("process_supervisor.sh")
	if err != nil {
		panic(err)
	}
	return string(data)
}()

// Argv zero the supervisor shell runs the script under
const supervisorArgv0 = "icebath-process-supervisor"

// Handle on a running supervisor, a fake in tests
type supervisorHandle interface {
	write(payload string) error
	closeStdin() error
	poll() (exited bool, code int)
	wait() int
	pid() int
}

// A supervisor that dies sooner than this after starting may not be able to stay up
var supervisorRestartMinUptime = 5 * time.Second

// An idle supervisor exits this long after the set last emptied
var supervisorIdleExit = 30 * time.Second

var (
	supervisorMu         sync.Mutex
	ownerSupervisor      supervisorHandle
	supervisedPgids      = map[int]bool{}
	startingYoungRestart bool
	idleRelease          *time.Timer
	spawnSupervisorFn    func() supervisorHandle
)

func init() { spawnSupervisorFn = spawnSupervisor }

// Reports whether the owner death supervisor currently covers the process group
func ProcessGroupSupervised(pgid int) bool {
	supervisorMu.Lock()
	defer supervisorMu.Unlock()
	return supervisedPgids[pgid]
}

// Starts the per process supervisor, nil when it cannot be started
func spawnSupervisor() supervisorHandle {
	h, err := startSupervisorProcess()
	if err != nil {
		identityLogger().Debug("Could not start the owner-death supervisor", "error", err)
		return nil
	}
	watchSupervisor(h)
	return h
}

// One waiter per supervisor restarting it with every group replayed when it died on its own
func watchSupervisor(h supervisorHandle) {
	started := time.Now()
	youngRestart := startingYoungRestart
	go func() {
		code := h.wait()
		supervisorMu.Lock()
		defer supervisorMu.Unlock()
		if ownerSupervisor != nil && ownerSupervisor != h {
			return
		}
		ownerSupervisor = nil
		pruneDeadSupervisedPgids()
		if len(supervisedPgids) == 0 {
			return
		}
		oldEnough := time.Since(started) >= supervisorRestartMinUptime
		youngSignal := !oldEnough && code < 0 && !youngRestart
		restart := oldEnough || youngSignal
		what := "it could not stay up, retrying at the next registration"
		if restart {
			what = "restarting it"
		}
		identityLogger().Warn(fmt.Sprintf("owner-death supervisor exited (status %d) with %d process group(s) registered; %s", code, len(supervisedPgids), what))
		if restart {
			startingYoungRestart = youngSignal
			startSupervisorLocked()
			startingYoungRestart = false
		}
	}()
}

// Closes the supervisor's pipe once the set has stayed empty for the idle grace, caller holds the lock
func releaseWhenIdleLocked(h supervisorHandle) {
	cancelIdleReleaseLocked()
	if supervisorIdleExit <= 0 {
		closeIdleSupervisorLocked(h)
		return
	}
	idleRelease = time.AfterFunc(supervisorIdleExit, func() {
		supervisorMu.Lock()
		defer supervisorMu.Unlock()
		closeIdleSupervisorLocked(h)
	})
}

func closeIdleSupervisorLocked(h supervisorHandle) {
	if ownerSupervisor != h || len(supervisedPgids) > 0 {
		return
	}
	h.closeStdin()
	ownerSupervisor = nil
}

func cancelIdleReleaseLocked() {
	if idleRelease != nil {
		idleRelease.Stop()
		idleRelease = nil
	}
}

// Starts a supervisor and replays every live registration to it, caller holds the lock
func startSupervisorLocked() supervisorHandle {
	h := spawnSupervisorFn()
	ownerSupervisor = h
	if h == nil {
		return nil
	}
	var payload strings.Builder
	for _, pgid := range sortedPgids(supervisedPgids) {
		fmt.Fprintf(&payload, "register %d\n", pgid)
	}
	if err := h.write(payload.String()); err != nil {
		ownerSupervisor = nil
		return nil
	}
	return h
}

func sortedPgids(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for pgid := range set {
		out = append(out, pgid)
	}
	sort.Ints(out)
	return out
}

// IsOwnerDeathSupervisor reports whether a direct child's argv is this install's supervisor shell
func IsOwnerDeathSupervisor(argv []string) bool {
	return len(argv) >= 4 && argv[1] == "-c" && argv[3] == supervisorArgv0 && argv[2] == supervisorScript
}

// OwnerDeathSupervisor is a supervisor shell spawned outside the shared slot, closed and reaped by its caller
type OwnerDeathSupervisor struct{ h supervisorHandle }

// Pid of the supervisor shell
func (s *OwnerDeathSupervisor) PID() int { return s.h.pid() }

// Closes the control pipe and waits for the shell to exit, returning its exit status
func (s *OwnerDeathSupervisor) CloseAndWait() int {
	_ = s.h.closeStdin()
	return s.h.wait()
}

// SpawnOwnerDeathSupervisor starts a watched supervisor without publishing it as this process's shared one
func SpawnOwnerDeathSupervisor() (*OwnerDeathSupervisor, error) {
	h, err := startSupervisorProcess()
	if err != nil {
		return nil, err
	}
	watchSupervisor(h)
	return &OwnerDeathSupervisor{h: h}, nil
}

// OwnerDeathSupervisorPID is the pid of the published supervisor, zero when none runs
func OwnerDeathSupervisorPID() int {
	supervisorMu.Lock()
	defer supervisorMu.Unlock()
	if ownerSupervisor == nil {
		return 0
	}
	return ownerSupervisor.pid()
}

// IsolateOwnerDeathSupervisor empties the supervisor slot with an immediate idle exit until the returned restore runs
func IsolateOwnerDeathSupervisor() (restore func()) {
	supervisorMu.Lock()
	defer supervisorMu.Unlock()
	prevSup, prevPgids, prevIdle := ownerSupervisor, supervisedPgids, supervisorIdleExit
	ownerSupervisor, supervisedPgids, supervisorIdleExit = nil, map[int]bool{}, 0
	return func() {
		supervisorMu.Lock()
		defer supervisorMu.Unlock()
		ownerSupervisor, supervisedPgids, supervisorIdleExit = prevSup, prevPgids, prevIdle
	}
}

// ProcessCmdline is the argv of a live process, an error once it is gone or unreadable
func ProcessCmdline(pid int) ([]string, error) { return identityProber.Cmdline(pid) }

// ProcessZombie reports whether pid exited and awaits its reaper
func ProcessZombie(pid int) bool { return identityProber.Zombie(pid) }

// ProcessChildren lists the direct children of pid from the process table
func ProcessChildren(pid int) []int { return identityProber.Children(pid, false) }

// Forgets supervised groups with no members left and returns what went, caller holds the lock
func pruneDeadSupervisedPgids() map[int]bool {
	stale := map[int]bool{}
	for pgid := range supervisedPgids {
		if processGroupGoneFn(pgid) {
			stale[pgid] = true
		}
	}
	for pgid := range stale {
		delete(supervisedPgids, pgid)
	}
	return stale
}

// Makes these process groups die with this process however it dies
func ownProcessGroups(pgids []int) { updateSupervisedPgids("register", pgids) }

// Stops covering these groups because this process stopped them itself
func releaseProcessGroups(pgids []int) { updateSupervisedPgids("unregister", pgids) }

// OwnProcessGroups makes these process groups die with this process however it dies
func OwnProcessGroups(pgids []int) { OwnProcessGroupsFn(pgids) }

// ReleaseProcessGroups stops covering these groups because this process stopped them itself
func ReleaseProcessGroups(pgids []int) { ReleaseProcessGroupsFn(pgids) }

// ReleaseProcessGroupOnceEmpty releases pgid once no member is left, polling the leader through poll
func ReleaseProcessGroupOnceEmpty(pgid int, poll func(), timeout time.Duration) bool {
	if isWindowsHost {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		if poll != nil {
			poll()
		}
		if processGroupGoneFn(pgid) {
			ReleaseProcessGroupsFn([]int{pgid})
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// StopOwnedGroupSurvivors stops what is left of owned group pgid after its leader exited, then releases it
func StopOwnedGroupSurvivors(pgid int, poll func(), grace time.Duration) bool {
	if isWindowsHost {
		return false
	}
	supervisorMu.Lock()
	owned := supervisedPgids[pgid]
	supervisorMu.Unlock()
	if !owned {
		return ReleaseProcessGroupOnceEmpty(pgid, poll, 0)
	}
	for _, step := range []struct {
		force bool
		wait  time.Duration
	}{{false, grace}, {true, time.Second}} {
		if ReleaseProcessGroupOnceEmpty(pgid, poll, 0) {
			return true
		}
		if err := signalProcessGroup(pgid, step.force); err != nil && !errors.Is(err, errNoSuchProcess) {
			return false
		}
		if ReleaseProcessGroupOnceEmpty(pgid, poll, step.wait) {
			return true
		}
	}
	return false
}

// SpawnOwned starts a long lived child that dies with this process and records it in the ledger
func SpawnOwned(ctx context.Context, cmd *exec.Cmd, purpose string) error {
	if isWindowsHost {
		return cmd.Start()
	}
	startNewSession(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	OwnProcessGroupsFn([]int{cmd.Process.Pid})
	RegisterChildFn(ctx, cmd.Process.Pid, purpose, "")
	return nil
}

// Sends register or unregister lines for pgids to this process's supervisor, failures never fail a child
func updateSupervisedPgids(verb string, pgids []int) {
	if isWindowsHost {
		return
	}
	wanted := map[int]bool{}
	for _, pgid := range pgids {
		wanted[pgid] = true
	}
	if len(wanted) == 0 {
		return
	}
	supervisorMu.Lock()
	defer supervisorMu.Unlock()
	for pgid := range wanted {
		if verb == "register" {
			supervisedPgids[pgid] = true
		} else {
			delete(supervisedPgids, pgid)
		}
	}
	stale := pruneDeadSupervisedPgids()
	h := ownerSupervisor
	exited := true
	if h != nil {
		exited, _ = h.poll()
	}
	if h == nil || exited {
		if len(supervisedPgids) == 0 {
			return
		}
		startSupervisorLocked()
		return
	}
	if len(supervisedPgids) > 0 {
		cancelIdleReleaseLocked()
	}
	var payload strings.Builder
	for _, pgid := range sortedPgids(wanted) {
		fmt.Fprintf(&payload, "%s %d\n", verb, pgid)
	}
	for _, pgid := range sortedPgids(stale) {
		fmt.Fprintf(&payload, "unregister %d\n", pgid)
	}
	if err := h.write(payload.String()); err != nil {
		ownerSupervisor = nil
		return
	}
	if len(supervisedPgids) == 0 {
		releaseWhenIdleLocked(h)
	}
}
