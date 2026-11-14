package proc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	jsonv1 "encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
)

const SpawnEnvVar = "ICEBATH_SPAWN"

const spawnTagVersion = "v1"

const (
	ledgerLockTimeout = 2 * time.Second
	epochSlack        = 2.0
	earlierBoot       = -1.0
	argvTokens        = 10
)

var ErrLedgerCorrupt = errors.New("spawn ledger is unreadable")

type SpawnTag struct {
	Install       string
	Purpose       string
	SpawnerPID    int
	SpawnerCreate *float64
}

func (t SpawnTag) String() string {
	create := "-"
	if t.SpawnerCreate != nil {
		create = strconv.FormatFloat(*t.SpawnerCreate, 'f', 3, 64)
	}
	return strings.Join([]string{spawnTagVersion, t.Install, t.Purpose, strconv.Itoa(t.SpawnerPID), create}, ":")
}

func ParseSpawnTag(raw string) (SpawnTag, bool) {
	parts := strings.Split(raw, ":")
	if len(parts) != 5 || parts[0] != spawnTagVersion || parts[1] == "" || parts[2] == "" {
		return SpawnTag{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(parts[3]))
	if err != nil {
		return SpawnTag{}, false
	}
	tag := SpawnTag{Install: parts[1], Purpose: parts[2], SpawnerPID: pid}
	if parts[4] != "-" {
		create, err := strconv.ParseFloat(strings.TrimSpace(parts[4]), 64)
		if err != nil {
			return SpawnTag{}, false
		}
		tag.SpawnerCreate = &create
	}
	return tag, pid > 0
}

func InstallID(root string) string {
	canonical := root
	if abs, err := filepath.Abs(root); err == nil {
		canonical = abs
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			canonical = real
		}
	}
	sum := sha256.Sum256([]byte(strings.ToLower(canonical)))
	return hex.EncodeToString(sum[:])[:12]
}

func SpawnedByThisProcess(pid int) bool {
	env, err := Environ(pid)
	if err != nil {
		return false
	}
	tag, ok := ParseSpawnTag(env[SpawnEnvVar])
	if !ok || tag.SpawnerPID != os.Getpid() {
		return false
	}
	mine, err := Started(os.Getpid())
	return tag.SpawnerCreate == nil || err != nil || math.Abs(*tag.SpawnerCreate-mine) < epochSlack
}

type LedgerEntry struct {
	PID           int      `json:"pid"`
	CreateTime    *float64 `json:"create_time"`
	Purpose       string   `json:"purpose"`
	Install       string   `json:"install"`
	SpawnerPID    *int     `json:"spawner_pid"`
	SpawnerCreate *float64 `json:"spawner_create"`
	RegisteredAt  float64  `json:"registered_at"`
	Argv          string   `json:"argv"`
	Host          string   `json:"host"`
	Port          *int     `json:"port"`
	Profile       string   `json:"profile"`
	HomeKey       string   `json:"icebath_home"`
	Isolated      bool     `json:"isolated"`
	BootStart     *float64 `json:"boot_start"`
	SpawnerBoot   *float64 `json:"spawner_boot"`
	BootID        *string  `json:"boot_id"`

	fields map[string]any
	raw    jsonv1.RawMessage
}

func (e LedgerEntry) StillRunning() bool {
	if e.PID <= 0 {
		return false
	}
	return incarnation(e.PID, e.fields["create_time"], entryBoot(e.fields, "boot_start")) != provenDead
}

func (e LedgerEntry) SpawnerDead() (dead, known bool) {
	pid, ok := jsonInt(e.fields["spawner_pid"])
	if !ok || pid <= 0 {
		return false, false
	}
	switch incarnation(pid, e.fields["spawner_create"], entryBoot(e.fields, "spawner_boot")) {
	case provenAlive:
		return false, true
	case provenDead:
		return true, true
	}
	return false, false
}

func AliveSince(pid int, created *float64) (alive, known bool) {
	var recorded any
	if created != nil {
		recorded = *created
	}
	switch incarnation(pid, recorded, nil) {
	case provenAlive:
		return true, true
	case provenDead:
		return false, true
	}
	return false, false
}

func ReadLedger(path string) ([]LedgerEntry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLedgerCorrupt, err)
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: %s is not UTF-8 text", ErrLedgerCorrupt, path)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	if lead := bytes.TrimLeft(data, " \t\r\n"); len(lead) == 0 || lead[0] != '[' {
		return nil, fmt.Errorf("%w: %s does not hold a list", ErrLedgerCorrupt, path)
	}
	var items []jsonv1.RawMessage
	if err := jsonv1.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrLedgerCorrupt, path, err)
	}
	entries := make([]LedgerEntry, 0, len(items))
	for _, raw := range items {
		if e, ok := decodeLedgerEntry(raw); ok {
			entries = append(entries, e)
		}
	}
	return entries, nil
}

func decodeLedgerEntry(raw jsonv1.RawMessage) (LedgerEntry, bool) {
	if lead := bytes.TrimLeft(raw, " \t\r\n"); len(lead) == 0 || lead[0] != '{' {
		return LedgerEntry{}, false
	}
	dec := jsonv1.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var fields map[string]any
	if dec.Decode(&fields) != nil {
		return LedgerEntry{}, false
	}
	e := LedgerEntry{fields: fields, raw: raw}
	e.PID, _ = jsonInt(fields["pid"])
	e.CreateTime = jsonFloat(fields["create_time"])
	e.Purpose, _ = fields["purpose"].(string)
	e.Install, _ = fields["install"].(string)
	if pid, ok := jsonInt(fields["spawner_pid"]); ok {
		e.SpawnerPID = &pid
	}
	e.SpawnerCreate = jsonFloat(fields["spawner_create"])
	if at := jsonFloat(fields["registered_at"]); at != nil {
		e.RegisteredAt = *at
	}
	e.Argv, _ = fields["argv"].(string)
	e.Host, _ = fields["host"].(string)
	if port, ok := jsonInt(fields["port"]); ok {
		e.Port = &port
	}
	e.Profile, _ = fields["profile"].(string)
	e.HomeKey, _ = fields["icebath_home"].(string)
	e.Isolated, _ = fields["isolated"].(bool)
	e.BootStart = jsonFloat(fields["boot_start"])
	e.SpawnerBoot = jsonFloat(fields["spawner_boot"])
	if id, ok := fields["boot_id"].(string); ok {
		e.BootID = &id
	}
	return e, true
}

func jsonInt(v any) (int, bool) {
	n, ok := v.(jsonv1.Number)
	if !ok || strings.ContainsAny(string(n), ".eE") {
		return 0, false
	}
	i, err := strconv.Atoi(string(n))
	return i, err == nil
}

func jsonFloat(v any) *float64 {
	n, ok := v.(jsonv1.Number)
	if !ok {
		return nil
	}
	f, err := n.Float64()
	if err != nil {
		return nil
	}
	return &f
}

func recordedNumber(v any) (value float64, set, ok bool) {
	switch n := v.(type) {
	case nil:
		return 0, false, true
	case float64:
		return n, true, true
	case jsonv1.Number:
		f, err := n.Float64()
		return f, true, err == nil
	}
	return 0, true, false
}

func entryBoot(fields map[string]any, key string) any {
	recorded, ok := fields["boot_id"].(string)
	live, err := BootID()
	if !ok || err != nil {
		return nil
	}
	if recorded == live {
		return fields[key]
	}
	return earlierBoot
}

type liveness int8

const (
	provenAlive liveness = iota
	provenDead
	unproven
)

func probeFailure(err error) liveness {
	if errors.Is(err, ErrNotFound) {
		return provenDead
	}
	return unproven
}

func sameBirth(pid int, createTime, boot any) liveness {
	if !Exists(pid) {
		return provenDead
	}
	same, decided := true, false
	if b, set, ok := recordedNumber(boot); set {
		if !ok {
			return unproven
		}
		live, err := SinceBoot(pid)
		switch {
		case err == nil:
			same, decided = live == b, true
		case !errors.Is(err, errors.ErrUnsupported):
			return probeFailure(err)
		}
	}
	if !decided {
		if c, set, ok := recordedNumber(createTime); set {
			if !ok {
				return unproven
			}
			started, err := Started(pid)
			if err != nil {
				return probeFailure(err)
			}
			same = math.Abs(started-c) < epochSlack
		}
	}
	if !same {
		return provenDead
	}
	return provenAlive
}

func incarnation(pid int, createTime, boot any) liveness {
	if pid < 0 {
		return unproven
	}
	same := sameBirth(pid, createTime, boot)
	if same != provenAlive || !zombiesExist {
		return same
	}
	st, err := StatusOf(pid)
	if err != nil {
		return probeFailure(err)
	}
	if st == Zombie {
		return provenDead
	}
	return provenAlive
}

func verifiedAlive(pid int, createTime any) bool {
	if !Exists(pid) {
		return false
	}
	c, set, ok := recordedNumber(createTime)
	if !set || !ok {
		return false
	}
	started, err := Started(pid)
	if err != nil || started != c {
		return false
	}
	if !zombiesExist {
		return true
	}
	st, err := StatusOf(pid)
	return err == nil && st != Zombie
}

func liveBootID() *string {
	id, err := BootID()
	if err != nil {
		return nil
	}
	return &id
}

func verifiedBootStart(pid int, created *float64) *float64 {
	if pid <= 0 || created == nil || liveBootID() == nil {
		return nil
	}
	started, err := Started(pid)
	if err != nil || math.Abs(started-*created) >= epochSlack {
		return nil
	}
	boot, err := SinceBoot(pid)
	if err != nil {
		return nil
	}
	return &boot
}

func startedAt(pid int) *float64 {
	started, err := Started(pid)
	if err != nil {
		return nil
	}
	return &started
}

func firstTokens(argv []string) string {
	return strings.Join(argv[:min(argvTokens, len(argv))], " ")
}

type Ledger struct {
	path     string
	lockPath string
	install  string

	mu         sync.Mutex
	lockWarned atomic.Bool
}

type LedgerFilter struct {
	AllInstalls  bool
	VerifiedOnly bool
}

type SelfRecord struct {
	HomeKey  string
	Host     string
	Port     int
	Profile  string
	Isolated bool
}

func NewLedger(path, lockPath, installRoot string) *Ledger {
	return &Ledger{path: path, lockPath: lockPath, install: InstallID(installRoot)}
}

func (l *Ledger) Install() string { return l.install }

func (l *Ledger) SpawnEnv(purpose string) string {
	self := os.Getpid()
	tag := SpawnTag{Install: l.install, Purpose: purpose, SpawnerPID: self, SpawnerCreate: startedAt(self)}
	return SpawnEnvVar + "=" + tag.String()
}

func desktopSpawner() (int, *float64) {
	pid, err := strconv.Atoi(strings.TrimSpace(os.Getenv("ICEBATH_PARENT_PID")))
	if err != nil || pid <= 0 {
		return 0, nil
	}
	ms, ok := strings.CutPrefix(os.Getenv("ICEBATH_PARENT_START_MARKER"), "winms:")
	if !ok {
		return pid, nil
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(ms), 64)
	if err != nil {
		return pid, nil
	}
	create := value / 1000.0
	return pid, &create
}

func (l *Ledger) newEntry(pid int, created *float64, purpose string, spawnerPID int, spawnerCreate *float64) *LedgerEntry {
	e := &LedgerEntry{
		PID: pid, CreateTime: created, Purpose: purpose, Install: l.install, SpawnerCreate: spawnerCreate,
		RegisteredAt: float64(time.Now().UnixNano()) / 1e9,
		BootStart:    verifiedBootStart(pid, created), BootID: liveBootID(),
	}
	if spawnerPID > 0 {
		e.SpawnerPID = &spawnerPID
	}
	e.SpawnerBoot = verifiedBootStart(spawnerPID, spawnerCreate)
	return e
}

func (l *Ledger) RegisterSelf(ctx context.Context, purpose string, rec SelfRecord) error {
	spawnerPID, spawnerCreate := desktopSpawner()
	if tag, ok := ParseSpawnTag(os.Getenv(SpawnEnvVar)); ok {
		spawnerPID, spawnerCreate = tag.SpawnerPID, tag.SpawnerCreate
	}
	self := os.Getpid()
	e := l.newEntry(self, startedAt(self), purpose, spawnerPID, spawnerCreate)
	e.HomeKey, e.Host, e.Profile, e.Isolated = rec.HomeKey, rec.Host, rec.Profile, rec.Isolated
	if rec.Port != 0 {
		port := rec.Port
		e.Port = &port
	}
	e.Argv = firstTokens(os.Args)
	return l.append(ctx, e)
}

func (l *Ledger) RegisterChild(ctx context.Context, pid int, purpose string) error {
	if pid <= 0 {
		return fmt.Errorf("spawn ledger: pid %d: %w", pid, ErrNotFound)
	}
	created, err := Started(pid)
	if err != nil {
		return fmt.Errorf("spawn ledger: %w", err)
	}
	self := os.Getpid()
	e := l.newEntry(pid, &created, purpose, self, startedAt(self))
	if argv, err := Cmdline(pid); err == nil {
		e.Argv = firstTokens(argv)
	}
	return l.append(ctx, e)
}

func (l *Ledger) readOrQuarantine() ([]LedgerEntry, error) {
	entries, err := ReadLedger(l.path)
	if errors.Is(err, ErrLedgerCorrupt) {
		parked := l.path + ".corrupt"
		if os.Rename(l.path, parked) == nil {
			slog.Warn(fmt.Sprintf("spawn ledger was unreadable; moved to %s", parked))
		}
	}
	return entries, err
}

func (l *Ledger) append(ctx context.Context, entry *LedgerEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	lockCtx, cancel := context.WithTimeout(ctx, ledgerLockTimeout)
	lock, err := fileutil.OpenLock(lockCtx, l.lockPath)
	cancel()
	if err != nil {
		if l.lockWarned.CompareAndSwap(false, true) {
			slog.Warn("spawn ledger lock unavailable (a stalled holder, or no file locks here); writing " +
				"without it, so concurrent icebath processes may drop each other's rows")
		}
		slog.Debug("spawn ledger lock unavailable", "err", err)
	} else {
		defer lock.Release()
	}
	entries, _ := l.readOrQuarantine()
	records := make([]any, 0, len(entries)+1)
	for _, e := range entries {
		if e.PID != entry.PID && e.StillRunning() {
			records = append(records, e.raw)
		}
	}
	records = append(records, entry)
	if err := os.MkdirAll(filepath.Dir(l.path), 0o777); err != nil {
		slog.Debug("spawn ledger write failed", "err", err)
		return fmt.Errorf("spawn ledger: %w", err)
	}
	style := fileutil.JSONStyle{Lines: true, Indent: 2, ASCII: true}
	if err := fileutil.WriteJSON(ctx, l.path, records, style, fileutil.Perm(0o600)); err != nil {
		slog.Debug("spawn ledger write failed", "err", err)
		return fmt.Errorf("spawn ledger: %w", err)
	}
	return nil
}

func (l *Ledger) Entries(filter LedgerFilter) []LedgerEntry {
	l.mu.Lock()
	entries, err := l.readOrQuarantine()
	l.mu.Unlock()
	if err != nil {
		return nil
	}
	var out []LedgerEntry
	for _, e := range entries {
		if _, isInt := jsonInt(e.fields["pid"]); !isInt || (!filter.AllInstalls && e.Install != l.install) {
			continue
		}
		if filter.VerifiedOnly {
			if !verifiedAlive(e.PID, e.fields["create_time"]) {
				continue
			}
		} else if incarnation(e.PID, e.fields["create_time"], entryBoot(e.fields, "boot_start")) == provenDead {
			continue
		}
		out = append(out, e)
	}
	return out
}
