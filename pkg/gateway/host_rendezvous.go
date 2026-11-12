package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	HostProtocolVersion = 1
	HostIdentityPath    = "/api/host/identity"
	HostProbeTimeout    = 2 * time.Second
	roleGateway         = "gateway"
	RoleGateway         = roleGateway
	RoleServe           = "serve"
	RoleDesktopServe    = "desktop-serve"
	legacyBootSlackS    = 120.0
)

var hostRoles = []string{roleGateway, RoleServe, RoleDesktopServe}

// Outcome of a host lock claim, an unopenable lock never reading as contention
type HostLockOutcome string

const (
	HostLockAcquired     HostLockOutcome = "acquired"
	HostLockHeldByOther  HostLockOutcome = "held-by-other"
	HostLockCouldNotOpen HostLockOutcome = "could-not-open"
)

// A published host role owner whose profiles are the served set, not the launch profile
type HostRecord struct {
	Role             string
	PID              int
	CreateTime       *float64
	Host             string
	Port             *int
	ProtocolVersion  int
	TokenFingerprint string
	Profiles         []string
	UpdatedAt        string
	Home             string
	StartTime        *int64
	BootEpoch        string
}

// Seams the rendezvous tests replace to simulate foreign owners, unreadable epochs and racing unlinks
var (
	hostGetuid           = os.Getuid
	hostCurrentBootEpoch = func() string { return CurrentInstantiationEpoch() }
	hostProcessStartTime = GetProcessStartTime
	hostProfilesToServe  = profiles.ProfilesToServe
	hostUnlink           = os.Remove
)

var (
	hostLockMu      sync.Mutex
	hostLockHandles = map[[2]string]*os.File{}
)

func hostLog() *slog.Logger { return slog.Default().With("logger", "gateway.host_rendezvous") }

// Record fields in their published key order
func (r *HostRecord) ToJSON() *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("role", r.Role)
	o.Set("home", r.Home)
	o.Set("pid", int64(r.PID))
	if r.CreateTime != nil {
		o.Set("createTime", *r.CreateTime)
	} else {
		o.Set("createTime", nil)
	}
	if r.StartTime != nil {
		o.Set("startTime", *r.StartTime)
	} else {
		o.Set("startTime", nil)
	}
	o.Set("bootEpoch", r.BootEpoch)
	o.Set("host", r.Host)
	if r.Port != nil {
		o.Set("port", int64(*r.Port))
	} else {
		o.Set("port", nil)
	}
	o.Set("protocolVersion", int64(r.ProtocolVersion))
	o.Set("tokenFingerprint", r.TokenFingerprint)
	ps := make([]any, len(r.Profiles))
	for i, p := range r.Profiles {
		ps[i] = p
	}
	o.Set("profiles", ps)
	o.Set("updatedAt", r.UpdatedAt)
	return o
}

// Python int value of a decoded JSON value when it is an int or bool
func jsonPyInt(v any) (*big.Int, bool) {
	switch x := v.(type) {
	case bool, int, int64, *big.Int:
		return pyIntOf(x)
	}
	return nil, false
}

// Python str of a value or empty when the value is falsy
func strOrEmpty(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Record from a decoded payload, nil unless it is a dict with a positive pid and a known role
func HostRecordFromJSON(payload any) *HostRecord {
	obj, ok := payload.(*pyjson.Object)
	if !ok {
		return nil
	}
	rawPID, _ := obj.Get("pid")
	rawRole, _ := obj.Get("role")
	pid, isInt := jsonPyInt(rawPID)
	role, isStr := rawRole.(string)
	if !isInt || pid.Sign() <= 0 || !pid.IsInt64() || pid.Int64() > math.MaxInt || !isStr || !validHostRole(role) {
		return nil
	}
	r := &HostRecord{Role: role, PID: int(pid.Int64())}
	if create, _ := obj.Get("createTime"); create != nil {
		if _, isBool := create.(bool); isBool {
			n, _ := jsonPyInt(create)
			f := float64(n.Int64())
			r.CreateTime = &f
		} else if n, ok := jsonPyInt(create); ok {
			f, _ := new(big.Float).SetInt(n).Float64()
			r.CreateTime = &f
		} else if f, ok := create.(float64); ok {
			r.CreateTime = &f
		}
	}
	if start, _ := obj.Get("startTime"); start != nil {
		if _, isBool := start.(bool); !isBool {
			if n, ok := jsonPyInt(start); ok && n.Sign() > 0 && n.IsInt64() {
				v := n.Int64()
				r.StartTime = &v
			}
		}
	}
	get := func(key string) any { v, _ := obj.Get(key); return v }
	r.BootEpoch = strOrEmpty(get("bootEpoch"))
	r.Host = strOrEmpty(get("host"))
	if n, ok := jsonPyInt(get("port")); ok && n.Sign() > 0 && n.Cmp(big.NewInt(65535)) <= 0 {
		p := int(n.Int64())
		r.Port = &p
	}
	if n, ok := jsonPyInt(get("protocolVersion")); ok && n.IsInt64() {
		r.ProtocolVersion = int(n.Int64())
	}
	r.TokenFingerprint = strOrEmpty(get("tokenFingerprint"))
	if list, ok := get("profiles").([]any); ok {
		r.Profiles = []string{}
		for _, p := range list {
			if s, ok := p.(string); ok {
				r.Profiles = append(r.Profiles, s)
			}
		}
	}
	r.UpdatedAt = strOrEmpty(get("updatedAt"))
	r.Home = strOrEmpty(get("home"))
	return r
}

// Per OS user rendezvous directory shared by every profile of this user
func HostStateDir() string { return GatewayLockDir() }

// Rendezvous directory created owner only and tightened when a umask left it wider
func EnsureHostStateDir(ctx context.Context) (string, error) {
	directory := HostStateDir()
	if _, err := home.MakeUserDir(ctx, directory, 0o700, true, true); err != nil {
		return "", err
	}
	tightenHostStateDir(directory)
	return directory, nil
}

func validHostRole(role string) bool {
	for _, r := range hostRoles {
		if r == role {
			return true
		}
	}
	return false
}

func validatedRole(role string) string {
	if !validHostRole(role) {
		panic(fmt.Errorf("unknown host role: %s", pyrepr.StrRepr(role)))
	}
	return role
}

func HostRecordPath(role string) string {
	return filepath.Join(HostStateDir(), "host-"+validatedRole(role)+".json")
}

func HostLockPath(role string) string {
	return filepath.Join(HostStateDir(), "host-"+validatedRole(role)+".lock")
}

func HostTokenPath(role string) string {
	return filepath.Join(HostStateDir(), "host-"+validatedRole(role)+".token")
}

// Short non reversible handle of a session token, safe to publish in the record
func HostTokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:16]
}

// Creation time of pid, this process when zero, nil when unknowable
func HostProcessCreateTime(pid int) *float64 { return tools.ProcessCreateTime(pid) }

// Spawn ledger liveness proof of pid against a create time, nil when it cannot say
func pidIncarnationMatches(pid int, createTime *float64) *bool {
	alive, known := tools.PidAliveMatches(pid, createTime, false, nil)
	if !known {
		return nil
	}
	return &alive
}

// Boot epoch in seconds, nil when unreadable
func hostBootTime() *float64 {
	boot, err := proc.BootTime()
	if err != nil {
		hostLog().Debug("boot time unreadable; legacy record age check skipped", "error", err)
		return nil
	}
	return &boot
}

// Whether nobody holds the role host lock, nil when it cannot be probed, never creating the lock file
func hostLockIsFree(role string) *bool {
	if OwnsHostLock(role) {
		return boolRef(false)
	}
	f, err := os.OpenFile(HostLockPath(role), os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return boolRef(true)
	}
	if err != nil {
		hostLog().Debug(fmt.Sprintf("host %s lock could not be probed", role), "error", err)
		return nil
	}
	return boolRef(!probeLockFile(f))
}

func boolRef(b bool) *bool { return &b }

func falseRef(b *bool) bool { return b != nil && !*b }

// Identity of this boot, empty when unreadable so a recorded epoch is then unprovable
func currentBootEpoch() string { return hostCurrentBootEpoch() }

// Whether the record still names the same live process incarnation, nil when unprovable
func recordIncarnationMatches(r *HostRecord) *bool {
	recorded := r.BootEpoch
	epoch := currentBootEpoch()
	if recorded != "" {
		if epoch == "" {
			return nil
		}
		if recorded != epoch {
			return boolRef(false)
		}
	}
	if r.StartTime == nil && r.CreateTime == nil {
		return boolRef(false)
	}
	var matched *bool
	if r.StartTime == nil {
		matched = pidIncarnationMatches(r.PID, r.CreateTime)
	} else {
		alive := pidIncarnationMatches(r.PID, nil)
		if alive == nil || !*alive {
			return alive
		}
		current := hostProcessStartTime(r.PID)
		if current != nil {
			ok, err := StartTimeFingerprintsMatch(*r.StartTime, current, StartTimeDriftTolerance)
			if err == nil {
				matched = &ok
			}
		}
	}
	if matched == nil || !*matched {
		return matched
	}
	if r.Role == roleGateway && recorded == "" && r.CreateTime != nil {
		if boot := hostBootTime(); boot != nil && *r.CreateTime < *boot-legacyBootSlackS {
			free := hostLockIsFree(r.Role)
			if free == nil {
				return nil
			}
			if *free {
				return boolRef(false)
			}
		}
	}
	return boolRef(true)
}

// A record nobody may attach to, absent, of an unknown protocol, dead or a reused pid
func HostRecordIsStale(r *HostRecord) bool {
	if r == nil || r.ProtocolVersion != HostProtocolVersion {
		return true
	}
	m := recordIncarnationMatches(r)
	return m != nil && !*m
}

// Whether the record incarnation probe positively matched, never on an unprovable answer
func HostLivenessIsProven(r *HostRecord) bool {
	if r.CreateTime == nil && r.StartTime == nil {
		return false
	}
	m := recordIncarnationMatches(r)
	return m != nil && *m
}

// Address to dial for the record, a wildcard bind reached over loopback
func HostDialHost(r *HostRecord) string {
	h := r.Host
	if h == "" {
		h = "127.0.0.1"
	}
	switch h {
	case "0.0.0.0", "::", "*", "":
		return "127.0.0.1"
	}
	return h
}

// Python equality of a decoded JSON value with an int
func jsonEqualsInt(v any, n int) bool {
	if i, ok := jsonPyInt(v); ok {
		return i.Cmp(big.NewInt(int64(n))) == 0
	}
	if f, ok := v.(float64); ok {
		return f == float64(n)
	}
	return false
}

// Owner identity the recorded endpoint answers over a bounded connect and token gated GET, nil otherwise
func ProbeHostOwner(r *HostRecord, timeout time.Duration) *pyjson.Object {
	if r.Port == nil || *r.Port == 0 {
		return nil
	}
	h := HostDialHost(r)
	addr := net.JoinHostPort(h, strconv.Itoa(*r.Port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		hostLog().Debug(fmt.Sprintf("host %s owner does not answer at %s:%d", r.Role, h, *r.Port))
		return nil
	}
	conn.Close()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+HostIdentityPath, nil)
	if err != nil {
		return nil
	}
	if token := ReadHostToken(r.Role); token != "" {
		req.Header.Set("X-Icebath-Token", token)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		hostLog().Debug(fmt.Sprintf("host %s identity probe failed at %s:%d", r.Role, h, *r.Port), "error", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil
	}
	decoded, err := pyjson.Loads(strings.ToValidUTF8(string(body), "\ufffd"))
	if err != nil {
		return nil
	}
	payload, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil
	}
	role, _ := payload.Get("role")
	pid, _ := payload.Get("pid")
	if s, isStr := role.(string); !isStr || s != r.Role || !jsonEqualsInt(pid, r.PID) {
		return nil
	}
	return payload
}

// Text of a file decoded as utf-8-sig, false when unreadable or not UTF-8
func readUTF8Sig(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(data) {
		return "", false
	}
	return strings.TrimPrefix(string(data), "\ufeff"), true
}

// Published record for role, nil when absent, foreign, corrupt or stale unless stale ones are wanted
func readHostRecord(role string, includeStale bool) *HostRecord {
	path := HostRecordPath(role)
	if !hostRecordIsOwn(path) {
		return nil
	}
	raw, ok := readUTF8Sig(path)
	if !ok {
		return nil
	}
	decoded, err := pyjson.Loads(raw)
	if err != nil {
		return nil
	}
	r := HostRecordFromJSON(decoded)
	if r == nil {
		return nil
	}
	if includeStale || !HostRecordIsStale(r) {
		return r
	}
	return nil
}

// Published record for role, nil when absent, foreign, corrupt or stale unless stale ones are wanted
func ReadHostRecord(role string, includeStale bool) *HostRecord {
	return readHostRecord(role, includeStale)
}

// Owner written session token for role, empty when absent or unreadable
func ReadHostToken(role string) string {
	text, ok := readUTF8Sig(HostTokenPath(role))
	if !ok {
		return ""
	}
	return pystr.Strip(text)
}

// Whether the on disk token still hashes to the record fingerprint, a tokenless record always consistent
func HostRecordTokenIsConsistent(r *HostRecord) bool {
	if r.TokenFingerprint == "" {
		return true
	}
	return HostTokenFingerprint(ReadHostToken(r.Role)) == r.TokenFingerprint
}

func hostLockKey(role string) [2]string {
	path := HostLockPath(role)
	if abs, err := filepath.Abs(path); err == nil {
		return [2]string{role, abs}
	}
	return [2]string{role, path}
}

// Takes the host wide lock for role idempotently per lock path, the open error accompanying could not open
func ClaimHostLock(ctx context.Context, role string) (HostLockOutcome, error) {
	role = validatedRole(role)
	key := hostLockKey(role)
	hostLockMu.Lock()
	defer hostLockMu.Unlock()
	if hostLockHandles[key] != nil {
		return HostLockAcquired, nil
	}
	path := key[1]
	if _, err := EnsureHostStateDir(ctx); err != nil {
		hostLog().Debug(fmt.Sprintf("host %s lock could not be opened at %s", role, path), "error", err)
		return HostLockCouldNotOpen, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		hostLog().Debug(fmt.Sprintf("host %s lock could not be opened at %s", role, path), "error", err)
		return HostLockCouldNotOpen, err
	}
	if !tryAcquireFileLock(f) {
		f.Close()
		return HostLockHeldByOther, nil
	}
	hostLockHandles[key] = f
	return HostLockAcquired, nil
}

// Releases the role host lock when this process holds it
func ReleaseHostLock(role string) {
	key := hostLockKey(validatedRole(role))
	hostLockMu.Lock()
	f := hostLockHandles[key]
	delete(hostLockHandles, key)
	hostLockMu.Unlock()
	if f == nil {
		return
	}
	releaseFileLock(f)
	f.Close()
}

// Whether this process holds the role host lock, re probing our own flock lying
func OwnsHostLock(role string) bool {
	key := hostLockKey(validatedRole(role))
	hostLockMu.Lock()
	defer hostLockMu.Unlock()
	return hostLockHandles[key] != nil
}

// Whether any process holds the role host lock, nil when the lock file cannot be opened
func HostLockIsHeld(role string) *bool {
	role = validatedRole(role)
	if OwnsHostLock(role) {
		return boolRef(true)
	}
	path := HostLockPath(role)
	if _, err := os.Stat(path); err != nil {
		return boolRef(false)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil
	}
	return boolRef(probeLockFile(f))
}

// Whether the record incarnation is proven and its host lock is not observed free
func recordNamesLiveOwner(r *HostRecord) bool {
	if !HostLivenessIsProven(r) {
		return false
	}
	if held := HostLockIsHeld(r.Role); held != nil && !*held {
		hostLog().Debug(fmt.Sprintf("host %s record names pid %d but the host lock is free", r.Role, r.PID))
		return false
	}
	return true
}

// Whether the record incarnation is proven and its host lock is not observed free
func RecordNamesLiveOwner(r *HostRecord) bool { return recordNamesLiveOwner(r) }

// Options of a host record publish, the token persisted 0600 beside it with only its fingerprint published
type HostPublishOptions struct {
	Host     string
	Port     *int
	Profiles []string
	Token    string
	Home     string
}

// Publishes this process as the role host owner, nil when the write failed
func PublishHostRecord(ctx context.Context, role string, o HostPublishOptions) *HostRecord {
	role = validatedRole(role)
	pid := os.Getpid()
	r := &HostRecord{
		Role:             role,
		PID:              pid,
		CreateTime:       HostProcessCreateTime(0),
		BootEpoch:        currentBootEpoch(),
		Host:             o.Host,
		ProtocolVersion:  HostProtocolVersion,
		TokenFingerprint: HostTokenFingerprint(o.Token),
		Profiles:         append([]string{}, o.Profiles...),
		UpdatedAt:        utcNowISO(),
		Home:             o.Home,
	}
	if n, ok := jsonPyInt(hostProcessStartTime(pid)); ok && n.IsInt64() {
		v := n.Int64()
		r.StartTime = &v
	}
	if o.Port != nil && *o.Port > 0 {
		p := *o.Port
		r.Port = &p
	}
	err := func() error {
		if _, err := EnsureHostStateDir(ctx); err != nil {
			return err
		}
		if o.Token != "" {
			if err := writePrivateText(HostTokenPath(role), o.Token); err != nil {
				return err
			}
		}
		return config.AtomicJSONWriteMode(ctx, HostRecordPath(role), r.ToJSON(), 0o600, pyjson.Indent(2))
	}()
	if err != nil {
		hostLog().Warn(fmt.Sprintf("host %s record could not be published; discovery will not find it", role), "error", err)
		return nil
	}
	invalidateHostGatewayCache()
	return r
}

// Removes the record and token files, ignoring any OS error
func unlinkHostRecordFiles(role string) {
	for _, path := range []string{HostRecordPath(role), HostTokenPath(role)} {
		_ = hostUnlink(path)
	}
}

// Retracts the role record once its owner is provably gone, holding the host lock across the re read and unlink
func DiscardDeadHostRecord(role string) bool {
	role = validatedRole(role)
	r := readHostRecord(role, true)
	if r == nil {
		return false
	}
	var guard *os.File
	if r.PID != os.Getpid() && !falseRef(recordIncarnationMatches(r)) {
		if OwnsHostLock(role) {
			return false
		}
		if guard = holdLockBriefly(role); guard == nil {
			return false
		}
	}
	retracted := func() bool {
		if guard != nil {
			defer func() {
				releaseFileLock(guard)
				guard.Close()
			}()
			if readHostRecord(role, true) == nil {
				return false
			}
		}
		unlinkHostRecordFiles(role)
		return true
	}()
	if !retracted {
		return false
	}
	invalidateHostGatewayCache()
	return true
}

// Opens and flocks the role host lock without registering it, nil when not won
func holdLockBriefly(role string) *os.File {
	f, err := os.OpenFile(HostLockPath(role), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil
	}
	if tryAcquireFileLock(f) {
		return f
	}
	f.Close()
	return nil
}

// Removes this process's record and token, never another owner's
func ClearHostRecord(role string) {
	role = validatedRole(role)
	if existing := readHostRecord(role, true); existing != nil && existing.PID != os.Getpid() {
		return
	}
	unlinkHostRecordFiles(role)
	invalidateHostGatewayCache()
}

var (
	hostCleanupMu    sync.Mutex
	hostCleanupRoles []string
)

// Clears the role record and releases its lock, surviving any panic like suppressed exceptions
func cleanupHostRole(role string) {
	func() {
		defer func() { recover() }()
		ClearHostRecord(role)
	}()
	func() {
		defer func() { recover() }()
		ReleaseHostLock(role)
	}()
}

// Runs every registered role cleanup, the exit hook cmd/icebath calls on the way out
func HostRendezvousAtExit() {
	hostCleanupMu.Lock()
	roles := append([]string{}, hostCleanupRoles...)
	hostCleanupMu.Unlock()
	for _, role := range roles {
		cleanupHostRole(role)
	}
}

// Registers the role for cleanup at exit and on a terminating signal, other signal handlers still running
func HostCleanupOnExit(role string) {
	role = validatedRole(role)
	hostCleanupMu.Lock()
	defer hostCleanupMu.Unlock()
	for _, r := range hostCleanupRoles {
		if r == role {
			return
		}
	}
	hostCleanupRoles = append(hostCleanupRoles, role)
	installHostSignalCleanup()
}

// Whether this process multiplexes, the retired explicit false never narrowing the claim time record
func multiplexProfilesEnabled() bool { return true }

// Profiles this process multiplexes, empty when the roster cannot be read
func HostServedProfiles(multiplex *bool) []string {
	enabled := multiplexProfilesEnabled()
	if multiplex != nil {
		enabled = *multiplex
	}
	roster, err := hostProfilesToServe(context.Background(), enabled, false, false)
	if err != nil {
		hostLog().Debug("served profile roster unavailable", "error", err)
		return []string{}
	}
	names := make([]string, 0, len(roster))
	for _, p := range roster {
		names = append(names, p.Name)
	}
	return names
}

// One line human description used by attach messages and conflict logs
func DescribeHostRecord(r *HostRecord) string {
	where := "no bound port"
	if r.Port != nil && *r.Port != 0 {
		h := r.Host
		if h == "" {
			h = "127.0.0.1"
		}
		where = fmt.Sprintf("%s:%d", h, *r.Port)
	}
	ps := "unknown"
	if len(r.Profiles) > 0 {
		ps = strings.Join(r.Profiles, ", ")
	}
	return fmt.Sprintf("PID %d (%s; profiles: %s)", r.PID, where, ps)
}
