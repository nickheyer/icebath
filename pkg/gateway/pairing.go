package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Unambiguous code alphabet without 0, O, 1 and I, and the code length
const (
	PairingAlphabet   = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	PairingCodeLength = 8
)

// Code lifetime, per user request spacing, lockout length, pending cap, failures before lockout and decline spacing
const (
	PairingCodeTTLSeconds        = 3600
	PairingRateLimitSeconds      = 600
	PairingLockoutSeconds        = 3600
	PairingMaxPendingPerPlatform = 3
	PairingMaxFailedAttempts     = 5
	PairingDeclineDedupeSeconds  = 24 * 3600
)

func pairingLog() *slog.Logger { return slog.Default().With("logger", "gateway.pairing") }

// Seconds since the epoch as time.time reports them
func pairingNow() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Pairing directory of the home bound to ctx, resolved on every call
func defaultPairingDir(ctx context.Context) string {
	return home.HermesDir(home.Dir(ctx), "platforms/pairing", "pairing")
}

// Allowlist env var name a platform declares, empty when it has none
func allowlistEnvForPlatform(ctx context.Context, platform string) (name string) {
	platform = pystr.Strip(strings.ToLower(platform))
	func() {
		defer func() { _ = recover() }()
		plugins.DiscoverPlugins(ctx, false)
	}()
	defer func() {
		if recover() != nil {
			name = ""
		}
	}()
	if value := AuthzField(platform, "allowed_users_env", Registry.Get(ctx, platform)); pyrepr.Truthy(value) {
		return pyrepr.Str(value)
	}
	return ""
}

func splitAllowlist(raw string) []string {
	ids := []string{}
	for _, uid := range strings.Split(raw, ",") {
		if uid = pystr.Strip(uid); uid != "" {
			ids = append(ids, uid)
		}
	}
	return ids
}

// True for the bridge and the cloud WhatsApp platforms, which share phone and JID identity rules
func platformUsesWhatsAppIdentity(platform string) bool {
	name := strings.ToLower(pystr.Strip(platform))
	return name == "whatsapp" || name == "whatsapp_cloud"
}

// User id as it is persisted and compared on a platform
func normalizePairingUserID(platform, userID string) string {
	raw := pystr.Strip(userID)
	if platformUsesWhatsAppIdentity(platform) {
		if normalized := NormalizeWhatsAppIdentifier(raw); normalized != "" {
			return normalized
		}
	}
	return raw
}

// Every equivalent user id of a principal for authorization and allowlist matching
func pairingUserIDAliases(ctx context.Context, platform, userID string) (map[string]bool, error) {
	raw := pystr.Strip(userID)
	aliases := map[string]bool{}
	if raw == "" {
		return aliases, nil
	}
	aliases[raw], aliases[normalizePairingUserID(platform, raw)] = true, true
	if platformUsesWhatsAppIdentity(platform) {
		expanded, err := expandWhatsAppAliases(ctx, raw)
		if err != nil {
			return nil, err
		}
		for alias := range expanded {
			aliases[alias] = true
		}
	}
	delete(aliases, "")
	return aliases, nil
}

// Whether two user ids name the same principal
func pairingUserIDsMatch(ctx context.Context, platform, left, right string) (bool, error) {
	leftAliases, err := pairingUserIDAliases(ctx, platform, left)
	if err != nil || len(leftAliases) == 0 {
		return false, err
	}
	rightAliases, err := pairingUserIDAliases(ctx, platform, right)
	if err != nil {
		return false, err
	}
	for alias := range leftAliases {
		if rightAliases[alias] {
			return true, nil
		}
	}
	return false, nil
}

// Approved ids that name the same principal as userID, in file order
func pairingMatchingIDs(ctx context.Context, platform string, approved *pyjson.Object, userID string) ([]string, error) {
	matching := []string{}
	for _, uid := range approved.Keys {
		same, err := pairingUserIDsMatch(ctx, platform, uid, userID)
		if err != nil {
			return nil, err
		}
		if same {
			matching = append(matching, uid)
		}
	}
	return matching, nil
}

// Allowlist env var read through the profile secret scope, a scoped miss reading as empty
func readAllowlistEnv(ctx context.Context, envVar string) (string, error) {
	value, err := GetScopedSecret(ctx, envVar, "", false)
	if err != nil {
		return "", err
	}
	return pystr.Strip(value), nil
}

// Env var and ids of a platform whose allowlist is configured, ok false for an open gateway
func configuredAllowlist(ctx context.Context, platform string) (envVar string, ids []string, ok bool, err error) {
	envVar = allowlistEnvForPlatform(ctx, platform)
	if envVar == "" {
		return "", nil, false, nil
	}
	current, err := readAllowlistEnv(ctx, envVar)
	if err != nil || current == "" {
		return "", nil, false, err
	}
	return envVar, splitAllowlist(current), true, nil
}

// Persists an allowlist, an empty list removing the key, failures leaving the pairing store as the grant record
func writeAllowlistEnv(ctx context.Context, envVar string, ids []string) {
	defer func() { _ = recover() }()
	if len(ids) > 0 {
		_ = config.SaveEnvValueContext(ctx, home.Dir(ctx), envVar, strings.Join(ids, ","))
		return
	}
	_, _ = config.RemoveHomeEnvValue(ctx, home.Dir(ctx), envVar)
}

// Adds a user to the platform allowlist env var when one is configured
func syncAllowlistAdd(ctx context.Context, platform, userID string) error {
	envVar, ids, ok, err := configuredAllowlist(ctx, platform)
	if err != nil || !ok {
		return err
	}
	for _, id := range ids {
		if id == "*" || id == userID {
			return nil
		}
	}
	writeAllowlistEnv(ctx, envVar, append(ids, userID))
	return nil
}

// Runner exposing the adapters of its primary profile
type primaryAdaptersRunner interface {
	Adapters() *pydict.Dict[Platform, PlatformAdapter]
}

// Adapters of the in process gateway runner across every served profile, none without a runner
func iterLiveGatewayAdapters() (adapters []PlatformAdapter) {
	defer func() {
		if recover() != nil {
			adapters = nil
		}
	}()
	runner := CurrentGatewayRunner()
	if runner == nil {
		return nil
	}
	if primary, ok := runner.(primaryAdaptersRunner); ok {
		for _, adapter := range primary.Adapters().All() {
			if adapter != nil {
				adapters = append(adapters, adapter)
			}
		}
	}
	if secondary, ok := runner.(servedProfileAdaptersRunner); ok {
		for _, mapping := range secondary.ProfileAdapters().All() {
			for _, adapter := range mapping.All() {
				if adapter != nil {
					adapters = append(adapters, adapter)
				}
			}
		}
	}
	return adapters
}

// Lower case platform value of an adapter, else its name
func adapterPlatformName(adapter PlatformAdapter) string {
	if base := adapter.Base(); base != nil && base.Platform != "" {
		return strings.ToLower(pystr.Strip(string(base.Platform)))
	}
	return strings.ToLower(pystr.Strip(adapter.Name()))
}

// Adapter keeping allowlist snapshots, a purge keeping only the entries the predicate accepts
type AllowlistSnapshotAdapter interface {
	PurgeAllowlistSnapshots(keep func(entry string) bool)
}

// Drops the entries equivalent to a user from an allowlist value while keeping the wildcard
func purgeAllowlistEntries(ctx context.Context, entries any, platform, userID string) any {
	keep := func(entry string) bool {
		if pystr.Strip(entry) == "*" {
			return true
		}
		same, err := pairingUserIDsMatch(ctx, platform, entry, userID)
		if err != nil {
			panic(err)
		}
		return !same
	}
	filter := func(items []string) []string {
		kept := []string{}
		for _, item := range items {
			if keep(item) {
				kept = append(kept, item)
			}
		}
		return kept
	}
	switch value := entries.(type) {
	case string:
		return strings.Join(filter(splitAllowlist(value)), ",")
	case map[string]bool:
		kept := map[string]bool{}
		for entry, present := range value {
			if present && keep(entry) {
				kept[entry] = true
			}
		}
		return kept
	case []string:
		return filter(value)
	case []any:
		kept := []any{}
		for _, item := range value {
			if keep(pyrepr.Str(item)) {
				kept = append(kept, item)
			}
		}
		return kept
	}
	return entries
}

// Clears a revoked principal from the allowlist snapshots of the running adapters of a platform
func syncLiveAdapterAllowlistRemove(ctx context.Context, platform, userID string) {
	platformName := strings.ToLower(pystr.Strip(platform))
	if platformName == "" || pystr.Strip(userID) == "" {
		return
	}
	keep := func(entry string) bool {
		if pystr.Strip(entry) == "*" {
			return true
		}
		same, err := pairingUserIDsMatch(ctx, platformName, entry, userID)
		if err != nil {
			panic(err)
		}
		return !same
	}
	for _, adapter := range iterLiveGatewayAdapters() {
		if adapterPlatformName(adapter) != platformName {
			continue
		}
		if holder, ok := adapter.(AllowlistSnapshotAdapter); ok {
			func() {
				defer func() { _ = recover() }()
				holder.PurgeAllowlistSnapshots(keep)
			}()
		}
		base := adapter.Base()
		if base == nil || base.Config == nil || base.Config.Extra == nil {
			continue
		}
		for _, key := range []string{"allow_from", "allowed_users"} {
			if current, present := base.Config.Extra[key]; present {
				func() {
					defer func() { _ = recover() }()
					base.Config.Extra[key] = purgeAllowlistEntries(ctx, current, platformName, userID)
				}()
			}
		}
	}
}

// Removes a user and its alias equivalents from the configured allowlist and the running adapters
func syncAllowlistRemove(ctx context.Context, platform, userID string) (err error) {
	envVar, ids, ok, err := configuredAllowlist(ctx, platform)
	if err != nil || !ok {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			asErr, isErr := r.(error)
			if !isErr {
				panic(r)
			}
			err = asErr
		}
	}()
	remaining := purgeAllowlistEntries(ctx, ids, platform, userID).([]string)
	if len(remaining) == len(ids) {
		return nil
	}
	writeAllowlistEnv(ctx, envVar, remaining)
	syncLiveAdapterAllowlistRemove(ctx, platform, userID)
	return nil
}

// JSON object of a file, empty when missing, malformed, unreadable or not an object, an error for undecodable bytes
func loadPairingJSONFile(path string) (*pyjson.Object, error) {
	if _, err := os.Stat(path); err != nil {
		return pyjson.NewObject(), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			ownerInfo := "<stat failed>"
			if st, statErr := os.Stat(path); statErr == nil {
				ownerInfo = fmt.Sprintf("owner_uid=%d mode=%04o", statOwnerUID(st), unixModeBits(st.Mode()))
			}
			pairingLog().Warn(fmt.Sprintf("Pairing file %s exists but is not readable as uid=%s (%s; %s). "+
				"If you ran `docker exec <container> icebath pairing approve ...` as root, "+
				"re-run with `docker exec -u icebath <container> ...` and "+
				"chown the existing file to the icebath user, or restart the "+
				"container so the entrypoint can fix ownership.", path, pairingEUID(), ownerInfo, pypath.OSErrorText(err)))
		}
		return pyjson.NewObject(), nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		return pyjson.NewObject(), nil
	}
	if object, ok := data.(*pyjson.Object); ok {
		return object, nil
	}
	return pyjson.NewObject(), nil
}

// Effective uid for the unreadable file warning, n/a where the platform has none
func pairingEUID() string {
	if runtime.GOOS == "windows" {
		return "n/a"
	}
	return strconv.Itoa(os.Geteuid())
}

// Permission, setuid, setgid and sticky bits of a mode as the last four octal digits of st_mode
func unixModeBits(mode os.FileMode) uint32 {
	bits := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if mode&os.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if mode&os.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

func savePairingJSONFile(ctx context.Context, path string, data *pyjson.Object) error {
	return config.AtomicJSONWriteMode(ctx, path, data, 0o600, pyjson.Indent(2))
}

// Merges the pairing data of the other layout into the active directory, active entries winning
func migrateSplitPairingDirs(ctx context.Context, h, active string) error {
	oldDir := filepath.Join(h, "pairing")
	activeResolved, err := pypath.Resolve(active)
	if err != nil {
		return err
	}
	oldResolved, err := pypath.Resolve(oldDir)
	if err != nil {
		return err
	}
	alternate := oldDir
	if activeResolved == oldResolved {
		alternate = filepath.Join(h, "platforms", "pairing")
	}
	alternateResolved, err := pypath.Resolve(alternate)
	if err != nil {
		return err
	}
	if _, err := os.Stat(alternate); err != nil || activeResolved == alternateResolved {
		return nil
	}
	if err := os.MkdirAll(active, 0o777); err != nil {
		return err
	}
	sources, _ := filepath.Glob(filepath.Join(alternate, "*.json"))
	for _, src := range sources {
		merged := pyjson.NewObject()
		if info, err := os.Stat(src); err == nil && info.Mode().IsRegular() {
			loaded, err := loadPairingJSONFile(src)
			if err != nil {
				return err
			}
			merged = loaded
		}
		if merged.Len() == 0 {
			continue
		}
		target := filepath.Join(active, filepath.Base(src))
		current, err := loadPairingJSONFile(target)
		if err != nil {
			return err
		}
		merged.Update(current)
		if !pyrepr.Equal(merged, current) {
			if err := savePairingJSONFile(ctx, target, merged); err != nil {
				return err
			}
		}
	}
	return nil
}

func isHashedPairingEntry(entry any) bool {
	object, ok := entry.(*pyjson.Object)
	if !ok {
		return false
	}
	_, hasSalt := object.Get("salt")
	_, hasHash := object.Get("hash")
	return hasSalt && hasHash
}

// Numeric created_at of a pending entry, false for malformed and legacy entries
func pairingEntryCreatedAt(info any) (float64, bool) {
	object, ok := info.(*pyjson.Object)
	if !ok {
		return 0, false
	}
	return pairingNumber(object.GetOr("created_at", nil))
}

// Float of an int, float or bool the way arithmetic reads them, false for anything else
func pairingNumber(v any) (float64, bool) {
	if flag, ok := v.(bool); ok {
		if flag {
			return 1, true
		}
		return 0, true
	}
	return pyFloatOf(v)
}

// Number a rate limit slot holds, zero for an absent slot and a TypeError for a value arithmetic refuses
func pairingLimit(limits *pyjson.Object, key string) (float64, error) {
	value, present := limits.Get(key)
	if !present {
		return 0, nil
	}
	if number, ok := pairingNumber(value); ok {
		return number, nil
	}
	return 0, &PyError{Kind: "TypeError", Msg: "unsupported operand type(s) for -: 'float' and '" + pyrepr.TypeName(value) + "'"}
}

type approvedCacheEntry struct {
	key  []any
	data *pyjson.Object
}

// Pairing codes and approved users of one pairing directory, shared by the adapters of a gateway
type PairingStore struct {
	dir           string
	mu            sync.Mutex
	profile       string
	scoped        bool
	approvedCache map[string]approvedCacheEntry
	// Reader of one store file, the module loader unless a caller of the store replaces it
	loadJSON func(path string) (*pyjson.Object, error)
}

// Store of the bound home, or of a named profile resolved the way `icebath -p <name> pairing` resolves it
func NewPairingStore(ctx context.Context, profile string) (*PairingStore, error) {
	if profile == "" {
		return newPairingStoreAt(ctx, defaultPairingDir(ctx), home.Dir(ctx), "")
	}
	root := home.Root()
	profileHome := root
	if profile != "default" {
		profileHome = filepath.Join(root, "profiles", profile)
	}
	return newPairingStoreAt(ctx, home.HermesDir(profileHome, "platforms/pairing", "pairing"), profileHome, profile)
}

// Store over an explicit directory, the other layout under the home merged into it first
func newPairingStoreAt(ctx context.Context, dir, h, profile string) (*PairingStore, error) {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, err
	}
	if err := migrateSplitPairingDirs(ctx, h, dir); err != nil {
		return nil, err
	}
	return &PairingStore{dir: dir, profile: profile, scoped: profile != "", approvedCache: map[string]approvedCacheEntry{},
		loadJSON: loadPairingJSONFile}, nil
}

// Profile name the store is scoped to, false for the store of the bound home
func (s *PairingStore) Profile() (string, bool) { return s.profile, s.scoped }

func (s *PairingStore) pendingPath(platform string) string {
	return filepath.Join(s.dir, platform+"-pending.json")
}

func (s *PairingStore) approvedPath(platform string) string {
	return filepath.Join(s.dir, platform+"-approved.json")
}

func (s *PairingStore) rateLimitPath() string { return filepath.Join(s.dir, "_rate_limits.json") }

func (s *PairingStore) declineStampPath() string { return filepath.Join(s.dir, "_declined.json") }

// Removes expired pending codes, an entry with no numeric created_at counting as expired
func (s *PairingStore) cleanupExpired(ctx context.Context, platform string) error {
	path := s.pendingPath(platform)
	pending, err := s.loadJSON(path)
	if err != nil {
		return err
	}
	now := pairingNow()
	live := pyjson.NewObject()
	for _, key := range pending.Keys {
		if created, ok := pairingEntryCreatedAt(pending.Vals[key]); ok && now-created <= PairingCodeTTLSeconds {
			live.Set(key, pending.Vals[key])
		}
	}
	if live.Len() != pending.Len() {
		return savePairingJSONFile(ctx, path, live)
	}
	return nil
}

// Takes the cross process directory lock and then the store lock, returning the release of both
func (s *PairingStore) locked() (func(), error) {
	release, err := provider.FileLock(filepath.Join(s.dir, ".lock"), 10*time.Second,
		fmt.Sprintf("Timed out waiting for the pairing store lock (%s)", s.dir))
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	return func() {
		s.mu.Unlock()
		release()
	}, nil
}

func (s *PairingStore) platforms(platform, suffix string) ([]string, error) {
	if platform != "" {
		return []string{platform}, nil
	}
	return s.allPlatforms(suffix)
}

// Approved users of a platform, parsed once per file version, an unreadable file failing closed as empty
func (s *PairingStore) loadApproved(platform string) (*pyjson.Object, error) {
	path := s.approvedPath(platform)
	data, key, err := s.readApproved(platform, path)
	if err != nil {
		delete(s.approvedCache, platform)
		return s.loadJSON(path)
	}
	s.approvedCache[platform] = approvedCacheEntry{key, data}
	return data, nil
}

// Opens the approved file and returns its cached or parsed object with the cache key of the opened file
func (s *PairingStore) readApproved(platform, path string) (*pyjson.Object, []any, error) {
	stream, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer stream.Close()
	info, err := stream.Stat()
	if err != nil {
		return nil, nil, err
	}
	dev, _, err := pyfs.LstatIdentity(path)
	if err != nil {
		return nil, nil, err
	}
	key := append([]any{dev}, config.FileCacheVersionOf(path, info)...)
	if cached, ok := s.approvedCache[platform]; ok && pyrepr.Equal(cached.key, key) {
		return cached.data, key, nil
	}
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(stream); err != nil {
		return nil, nil, err
	}
	parsed, err := pyjson.LoadsBytes(raw.Bytes())
	if err != nil {
		return nil, nil, err
	}
	data, ok := parsed.(*pyjson.Object)
	if !ok {
		data = pyjson.NewObject()
	}
	return data, key, nil
}

// Whether a user is approved on a platform under any of its aliases
func (s *PairingStore) IsApproved(ctx context.Context, platform, userID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	approved, err := s.loadApproved(platform)
	if err != nil {
		return false, err
	}
	matching, err := pairingMatchingIDs(ctx, platform, approved, userID)
	return len(matching) > 0, err
}

// Approved users as rows of platform, user_id and the stored fields, of one platform or of all
func (s *PairingStore) ListApproved(platform string) ([]*pyjson.Object, error) {
	platforms, err := s.platforms(platform, "approved")
	if err != nil {
		return nil, err
	}
	rows := []*pyjson.Object{}
	for _, p := range platforms {
		approved, err := s.loadJSON(s.approvedPath(p))
		if err != nil {
			return nil, err
		}
		for _, uid := range approved.Keys {
			row := pyjson.ObjectOf("platform", p, "user_id", uid)
			info, ok := approved.Vals[uid].(*pyjson.Object)
			if !ok {
				return nil, &PyError{Kind: "TypeError", Msg: "'" + pyrepr.TypeName(approved.Vals[uid]) + "' object is not a mapping"}
			}
			row.Update(info)
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// Adds a user to the approved list and mirrors the grant into the allowlist, the store lock held
func (s *PairingStore) approveUser(ctx context.Context, platform, userID, userName string) error {
	approved, err := s.loadJSON(s.approvedPath(platform))
	if err != nil {
		return err
	}
	normalized := normalizePairingUserID(platform, userID)
	matching, err := pairingMatchingIDs(ctx, platform, approved, normalized)
	if err != nil {
		return err
	}
	for _, uid := range matching {
		approved.Delete(uid)
	}
	approved.Set(normalized, pyjson.ObjectOf("user_name", userName, "approved_at", pairingNow()))
	if err := savePairingJSONFile(ctx, s.approvedPath(platform), approved); err != nil {
		return err
	}
	return syncAllowlistAdd(ctx, platform, normalized)
}

// Removes a user from the approved list, true when the user was found
func (s *PairingStore) Revoke(ctx context.Context, platform, userID string) (bool, error) {
	path := s.approvedPath(platform)
	release, err := s.locked()
	if err != nil {
		return false, err
	}
	defer release()
	approved, err := s.loadJSON(path)
	if err != nil {
		return false, err
	}
	matching, err := pairingMatchingIDs(ctx, platform, approved, userID)
	if err != nil || len(matching) == 0 {
		return false, err
	}
	for _, uid := range matching {
		approved.Delete(uid)
	}
	if err := savePairingJSONFile(ctx, path, approved); err != nil {
		return false, err
	}
	return true, syncAllowlistRemove(ctx, platform, userID)
}

func hashPairingCode(code string, salt []byte) string {
	sum := sha256.Sum256(append(append([]byte{}, salt...), code...))
	return hex.EncodeToString(sum[:])
}

// Approved identity of a settled pairing request
type PairingApproval struct {
	UserID, UserName string
}

// Removes a pending request and approves its user, the store lock held
func (s *PairingStore) finishApproval(ctx context.Context, platform string, pending *pyjson.Object, matchedKey string, matched *pyjson.Object) (*PairingApproval, error) {
	pending.Delete(matchedKey)
	if err := savePairingJSONFile(ctx, s.pendingPath(platform), pending); err != nil {
		return nil, err
	}
	if err := s.resetFailedAttempts(ctx, platform); err != nil {
		return nil, err
	}
	userID, present := matched.Get("user_id")
	if !present {
		return nil, &PyError{Kind: "KeyError", Msg: "'user_id'"}
	}
	result := &PairingApproval{UserID: pyrepr.Str(userID), UserName: pyrepr.Str(matched.GetOr("user_name", ""))}
	return result, s.approveUser(ctx, platform, result.UserID, result.UserName)
}

// Uniform random text over an alphabet whose length divides 256
func randomPairingText(alphabet string, length int) string {
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	out := make([]byte, length)
	for i, b := range raw {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
}

// New pairing code for a user, false when rate limited, locked out or the platform is at its pending cap
func (s *PairingStore) GenerateCode(ctx context.Context, platform, userID, userName string) (string, bool, error) {
	release, err := s.locked()
	if err != nil {
		return "", false, err
	}
	defer release()
	if err := s.cleanupExpired(ctx, platform); err != nil {
		return "", false, err
	}
	normalized := normalizePairingUserID(platform, userID)
	lockedOut, err := s.isLockedOut(platform)
	if err != nil {
		return "", false, err
	}
	if !lockedOut {
		limited, err := s.isRateLimited(ctx, platform, userID)
		if err != nil {
			return "", false, err
		}
		lockedOut = limited
	}
	if lockedOut {
		return "", false, nil
	}
	pending, err := s.loadJSON(s.pendingPath(platform))
	if err != nil {
		return "", false, err
	}
	if pending.Len() >= PairingMaxPendingPerPlatform {
		return "", false, nil
	}
	code := randomPairingText(PairingAlphabet, PairingCodeLength)
	salt := make([]byte, 16)
	entryID := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return "", false, err
	}
	if _, err := rand.Read(entryID); err != nil {
		return "", false, err
	}
	pending.Set(hex.EncodeToString(entryID), pyjson.ObjectOf("hash", hashPairingCode(code, salt), "salt", hex.EncodeToString(salt),
		"user_id", normalized, "user_name", userName, "created_at", pairingNow()))
	if err := savePairingJSONFile(ctx, s.pendingPath(platform), pending); err != nil {
		return "", false, err
	}
	if err := s.recordRateLimit(ctx, platform, userID); err != nil {
		return "", false, err
	}
	return code, true, nil
}

// Approves a pairing code, nil when the code is invalid or expired or the platform is locked out
func (s *PairingStore) ApproveCode(ctx context.Context, platform, code string) (*PairingApproval, error) {
	release, err := s.locked()
	if err != nil {
		return nil, err
	}
	defer release()
	if err := s.cleanupExpired(ctx, platform); err != nil {
		return nil, err
	}
	code = strings.Join(pystr.SplitWhitespace(strings.ToUpper(code), -1), "")
	if lockedOut, err := s.isLockedOut(platform); err != nil || lockedOut {
		return nil, err
	}
	pending, err := s.loadJSON(s.pendingPath(platform))
	if err != nil {
		return nil, err
	}
	for _, entryID := range pending.Keys {
		entry := pending.Vals[entryID]
		if !isHashedPairingEntry(entry) {
			continue
		}
		object := entry.(*pyjson.Object)
		saltText, ok := object.GetOr("salt", nil).(string)
		if !ok {
			return nil, &PyError{Kind: "TypeError", Msg: "fromhex() argument must be str, not " + pyrepr.TypeName(object.GetOr("salt", nil))}
		}
		salt, err := pystr.BytesFromHex(saltText)
		if err != nil {
			continue
		}
		stored, ok := object.GetOr("hash", nil).(string)
		if !ok {
			return nil, &PyError{Kind: "TypeError", Msg: "unsupported operand types(s) or combination of types: 'str' and '" + pyrepr.TypeName(object.GetOr("hash", nil)) + "'"}
		}
		if subtle.ConstantTimeCompare([]byte(hashPairingCode(code, salt)), []byte(stored)) == 1 {
			return s.finishApproval(ctx, platform, pending, entryID, object)
		}
	}
	return nil, s.recordFailedAttempt(ctx, platform)
}

// Whether a value is shaped like a pending request id, sixteen hex characters
func LooksLikePairingRequestID(value string) bool {
	value = pystr.Strip(value)
	if len(value) != 16 {
		return false
	}
	for _, c := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// Approves a pending request by its request id, outside the brute force lockout, nil when no row has it
func (s *PairingStore) ApproveRequest(ctx context.Context, platform, requestID string) (*PairingApproval, error) {
	release, err := s.locked()
	if err != nil {
		return nil, err
	}
	defer release()
	if err := s.cleanupExpired(ctx, platform); err != nil {
		return nil, err
	}
	requestID = strings.ToLower(pystr.Strip(requestID))
	if requestID == "" {
		return nil, nil
	}
	pending, err := s.loadJSON(s.pendingPath(platform))
	if err != nil {
		return nil, err
	}
	for _, entryID := range pending.Keys {
		entry := pending.Vals[entryID]
		if isHashedPairingEntry(entry) && subtle.ConstantTimeCompare([]byte(strings.ToLower(entryID)), []byte(requestID)) == 1 {
			return s.finishApproval(ctx, platform, pending, entryID, entry.(*pyjson.Object))
		}
	}
	return nil, nil
}

// Pending request row as the list surfaces show it, the code itself never included
type PendingPairing struct {
	Platform, RequestID string
	UserID, UserName    any
	AgeMinutes          int
}

// Pending requests of one platform or of all, legacy entries reporting an empty request id
func (s *PairingStore) ListPending(ctx context.Context, platform string) ([]PendingPairing, error) {
	results := []PendingPairing{}
	release, err := s.locked()
	if err != nil {
		return nil, err
	}
	defer release()
	platforms, err := s.platforms(platform, "pending")
	if err != nil {
		return nil, err
	}
	for _, p := range platforms {
		if err := s.cleanupExpired(ctx, p); err != nil {
			return nil, err
		}
		pending, err := s.loadJSON(s.pendingPath(p))
		if err != nil {
			return nil, err
		}
		for _, entryID := range pending.Keys {
			info := pending.Vals[entryID]
			createdAt, ok := pairingEntryCreatedAt(info)
			if !ok {
				continue
			}
			object := info.(*pyjson.Object)
			_, hashIsText := object.GetOr("hash", nil).(string)
			_, saltIsText := object.GetOr("salt", nil).(string)
			requestID := ""
			if hashIsText && saltIsText {
				requestID = entryID
			}
			results = append(results, PendingPairing{Platform: p, RequestID: requestID, UserID: object.GetOr("user_id", ""),
				UserName: object.GetOr("user_name", ""), AgeMinutes: int((pairingNow() - createdAt) / 60)})
		}
	}
	return results, nil
}

// Clears the pending requests of one platform or of all, returning how many were removed
func (s *PairingStore) ClearPending(ctx context.Context, platform string) (int, error) {
	release, err := s.locked()
	if err != nil {
		return 0, err
	}
	defer release()
	platforms, err := s.platforms(platform, "pending")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, p := range platforms {
		pending, err := s.loadJSON(s.pendingPath(p))
		if err != nil {
			return 0, err
		}
		count += pending.Len()
		if err := savePairingJSONFile(ctx, s.pendingPath(p), pyjson.NewObject()); err != nil {
			return 0, err
		}
	}
	return count, nil
}

func (s *PairingStore) limits() (*pyjson.Object, error) {
	return s.loadJSON(s.rateLimitPath())
}

func (s *PairingStore) saveLimits(ctx context.Context, limits *pyjson.Object) error {
	return savePairingJSONFile(ctx, s.rateLimitPath(), limits)
}

// Whether a user under any alias requested a code too recently
func (s *PairingStore) isRateLimited(ctx context.Context, platform, userID string) (bool, error) {
	limits, err := s.limits()
	if err != nil {
		return false, err
	}
	aliases, err := pairingUserIDAliases(ctx, platform, userID)
	if err != nil {
		return false, err
	}
	for alias := range aliases {
		last, err := pairingLimit(limits, platform+":"+alias)
		if err != nil {
			return false, err
		}
		if pairingNow()-last < PairingRateLimitSeconds {
			return true, nil
		}
	}
	return false, nil
}

func (s *PairingStore) recordRateLimit(ctx context.Context, platform, userID string) error {
	limits, err := s.limits()
	if err != nil {
		return err
	}
	aliases, err := pairingUserIDAliases(ctx, platform, userID)
	if err != nil {
		return err
	}
	now := pairingNow()
	for _, alias := range sortedAliasKeys(aliases) {
		limits.Set(platform+":"+alias, now)
	}
	return s.saveLimits(ctx, limits)
}

// Whether a sender under any alias was sent a decline within the decline spacing
func (s *PairingStore) HasRecentDecline(ctx context.Context, platform, userID string) (bool, error) {
	stamps, err := s.loadJSON(s.declineStampPath())
	if err != nil {
		return false, err
	}
	aliases, err := pairingUserIDAliases(ctx, platform, userID)
	if err != nil {
		return false, err
	}
	now := pairingNow()
	for alias := range aliases {
		if stamped, ok := pairingNumber(stamps.GetOr(platform+":"+alias, nil)); ok && now-stamped < PairingDeclineDedupeSeconds {
			return true, nil
		}
	}
	return false, nil
}

// Stamps a sender under every alias as declined now and prunes the expired stamps
func (s *PairingStore) RecordDecline(ctx context.Context, platform, userID string) error {
	release, err := s.locked()
	if err != nil {
		return err
	}
	defer release()
	loaded, err := s.loadJSON(s.declineStampPath())
	if err != nil {
		return err
	}
	now := pairingNow()
	stamps := pyjson.NewObject()
	for _, key := range loaded.Keys {
		if value, ok := pairingNumber(loaded.Vals[key]); ok && now-value < PairingDeclineDedupeSeconds {
			stamps.Set(key, loaded.Vals[key])
		}
	}
	aliases, err := pairingUserIDAliases(ctx, platform, userID)
	if err != nil {
		return err
	}
	for _, alias := range sortedAliasKeys(aliases) {
		stamps.Set(platform+":"+alias, now)
	}
	return savePairingJSONFile(ctx, s.declineStampPath(), stamps)
}

// Aliases in a stable order for writing
func sortedAliasKeys(aliases map[string]bool) []string {
	keys := make([]string, 0, len(aliases))
	for alias := range aliases {
		keys = append(keys, alias)
	}
	slices.Sort(keys)
	return keys
}

// Whether approvals on a platform are locked out after too many failures
func (s *PairingStore) isLockedOut(platform string) (bool, error) {
	limits, err := s.limits()
	if err != nil {
		return false, err
	}
	until, err := pairingLimit(limits, "_lockout:"+platform)
	return pairingNow() < until, err
}

// Counts a failed approval and locks the platform out once the failures reach the cap
func (s *PairingStore) recordFailedAttempt(ctx context.Context, platform string) error {
	limits, err := s.limits()
	if err != nil {
		return err
	}
	failKey := "_failures:" + platform
	fails, err := pairingLimit(limits, failKey)
	if err != nil {
		return err
	}
	count := int(fails) + 1
	limits.Set(failKey, count)
	if count >= PairingMaxFailedAttempts {
		limits.Set("_lockout:"+platform, pairingNow()+PairingLockoutSeconds)
		limits.Set(failKey, 0)
		fmt.Fprintf(os.Stdout, "[pairing] Platform %s locked out for %ss after %s failed attempts\n", platform,
			strconv.Itoa(PairingLockoutSeconds), strconv.Itoa(PairingMaxFailedAttempts))
	}
	return s.saveLimits(ctx, limits)
}

// Clears the consecutive failure counter after a successful approval
func (s *PairingStore) resetFailedAttempts(ctx context.Context, platform string) error {
	limits, err := s.limits()
	if err != nil {
		return err
	}
	failKey := "_failures:" + platform
	if pyrepr.Truthy(limits.GetOr(failKey, nil)) {
		limits.Set(failKey, 0)
		return s.saveLimits(ctx, limits)
	}
	return nil
}

// Platforms that have a data file of the suffix, shared state files with a leading underscore left out
func (s *PairingStore) allPlatforms(suffix string) ([]string, error) {
	tail := "-" + suffix + ".json"
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	platforms := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, tail) {
			continue
		}
		if platform := strings.ReplaceAll(name, tail, ""); !strings.HasPrefix(platform, "_") {
			platforms = append(platforms, platform)
		}
	}
	return platforms, nil
}
