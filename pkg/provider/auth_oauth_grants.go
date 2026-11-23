package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

var oauthTokenFields = []string{"access_token", "refresh_token", "expires_at", "expires_at_ms", "last_refresh"}

const oauthHealCleanMarkFilename = "oauth_heal_clean.json"

var (
	oauthHealMu         sync.Mutex
	oauthHealCleanMarks = map[string]string{}
	oauthHealNotices    []string
)

// Python str of a value after the falsy or fallback to the empty string
func orEmptyStr(v any) string {
	if !truthyAny(v) {
		return ""
	}
	return anyString(v)
}

// OAuth by auth_type, a refresh token or an Anthropic OAuth access token
func isOAuthPoolPayload(entry any) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	return strings.ToLower(strings.TrimSpace(orEmptyStr(m["auth_type"]))) == AuthTypeOAuth ||
		strings.TrimSpace(orEmptyStr(m["refresh_token"])) != "" ||
		strings.HasPrefix(orEmptyStr(m["access_token"]), "sk-ant-oat")
}

func listOrEmpty(v any) []any {
	if rows, ok := v.([]any); ok {
		return rows
	}
	return []any{}
}

func nonemptyRowID(row any) (string, bool) {
	m, ok := row.(map[string]any)
	if !ok {
		return "", false
	}
	id, ok := m["id"].(string)
	return id, ok && id != ""
}

func sortedSingleUseProviders() []string {
	out := make([]string, 0, len(SingleUseRefreshPoolProviders))
	for p := range SingleUseRefreshPoolProviders {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// MergeSnapshotAuthPreservingLiveSingleUseGrants restores a snapshot store without reviving spent single use OAuth generations
func MergeSnapshotAuthPreservingLiveSingleUseGrants(snapshot, live map[string]any) map[string]any {
	restored, _ := deepCopyAny(snapshot).(map[string]any)
	if restored == nil {
		restored = map[string]any{}
	}
	snapshotPool, _ := restored["credential_pool"].(map[string]any)
	livePool, _ := live["credential_pool"].(map[string]any)
	for _, providerID := range sortedSingleUseProviders() {
		liveRows := listOrEmpty(livePool[providerID])
		snapshotRows := listOrEmpty(snapshotPool[providerID])
		var liveGrants []any
		for _, row := range liveRows {
			if isOAuthPoolPayload(row) {
				liveGrants = append(liveGrants, row)
			}
		}
		snapshotHasGrants := false
		for _, row := range snapshotRows {
			if isOAuthPoolPayload(row) {
				snapshotHasGrants = true
				break
			}
		}
		if len(liveGrants) == 0 && !snapshotHasGrants {
			continue
		}
		if snapshotPool == nil {
			snapshotPool = map[string]any{}
			restored["credential_pool"] = snapshotPool
		}
		var liveOrder []string
		liveByID := map[string]map[string]any{}
		for _, row := range liveRows {
			if id, ok := nonemptyRowID(row); ok {
				if _, seen := liveByID[id]; !seen {
					liveOrder = append(liveOrder, id)
				}
				liveByID[id] = row.(map[string]any)
			}
		}
		merged := []any{}
		for _, row := range liveGrants {
			if _, ok := nonemptyRowID(row); !ok {
				merged = append(merged, deepCopyAny(row))
			}
		}
		var mergedSnapshot []any
		consumed := map[string]bool{}
		for _, snapshotRow := range snapshotRows {
			rowID, hasID := nonemptyRowID(snapshotRow)
			var liveRow map[string]any
			if hasID {
				liveRow = liveByID[rowID]
			}
			liveIsGrant := liveRow != nil && isOAuthPoolPayload(liveRow)
			if isOAuthPoolPayload(snapshotRow) {
				if liveRow == nil || consumed[rowID] {
					continue
				}
				consumed[rowID] = true
				if liveIsGrant {
					snap := snapshotRow.(map[string]any)
					base := credentialTokenPair(snap)
					liveCopy, _ := deepCopyAny(liveRow).(map[string]any)
					mergedSnapshot = append(mergedSnapshot, mergePoolRowGeneration(snap, liveCopy, providerID, &base, false))
				} else {
					mergedSnapshot = append(mergedSnapshot, deepCopyAny(liveRow))
				}
				continue
			}
			if liveIsGrant {
				if consumed[rowID] {
					continue
				}
				consumed[rowID] = true
				mergedSnapshot = append(mergedSnapshot, deepCopyAny(liveRow))
			} else {
				mergedSnapshot = append(mergedSnapshot, snapshotRow)
			}
		}
		for _, id := range liveOrder {
			if row := liveByID[id]; isOAuthPoolPayload(row) && !consumed[id] {
				merged = append(merged, deepCopyAny(row))
			}
		}
		snapshotPool[providerID] = append(merged, mergedSnapshot...)
	}
	return restored
}

// Process default root the clone strip compares against, swappable so tests can make it fail
var stripDefaultRootFn = func() (string, error) { return home.Root(), nil }

// Hardlink identity of two existing paths, swappable so tests can make the stat fail
var stripSameFileFn = func(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(sa, sb), nil
}

func readJSONObjectFile(path string) (map[string]any, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var obj map[string]any
	if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &obj) != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// StripClonedSingleUseOAuthGrants removes forked single use OAuth logins from a freshly cloned profile, keeping API key rows, and names the pool keys it stripped
func StripClonedSingleUseOAuthGrants(profileDir string) []string {
	stripped := []string{}
	authPath := filepath.Join(profileDir, "auth.json")
	if st, err := os.Stat(authPath); err != nil || !st.Mode().IsRegular() {
		return stripped
	}
	rootDir, err := stripDefaultRootFn()
	if err != nil {
		return stripped
	}
	rootAuthPath := home.AuthPath(rootDir)
	if samePath(authPath, rootAuthPath) {
		return stripped
	}
	if _, err := os.Stat(rootAuthPath); err == nil {
		same, err := stripSameFileFn(authPath, rootAuthPath)
		if err != nil || same {
			return stripped
		}
	}
	store, ok := readJSONObjectFile(authPath)
	if !ok {
		return stripped
	}
	pool, ok := store["credential_pool"].(map[string]any)
	if !ok {
		return stripped
	}
	ids := make([]string, 0, len(pool))
	for id := range pool {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, providerID := range ids {
		entries, isList := pool[providerID].([]any)
		if !SingleUseRefreshPoolProviders[providerID] || !isList {
			continue
		}
		kept := []any{}
		for _, e := range entries {
			if !isOAuthPoolPayload(e) {
				kept = append(kept, e)
			}
		}
		if len(kept) != len(entries) {
			stripped = append(stripped, providerID)
			if len(kept) > 0 {
				pool[providerID] = kept
			} else {
				delete(pool, providerID)
			}
		}
	}
	if len(stripped) == 0 {
		return stripped
	}
	if err := NewStore(profileDir).SaveTo(AuthData(store), authPath); err != nil {
		pkgLog().Debug(fmt.Sprintf("Failed to strip cloned single-use OAuth grants from %s: %v", authPath, err))
	}
	return stripped
}

// Fingerprint as JSON text so in memory and on disk marks compare exactly
func oauthHealFingerprintJSON(fingerprint []any) string {
	raw, _ := json.Marshal(fingerprint)
	return string(raw)
}

func oauthHealCleanMarkPath(env PoolEnv) string {
	return filepath.Join(filepath.Dir(env.Store.Path()), "cache", oauthHealCleanMarkFilename)
}

// Clean mark file decoded with exact integers, nil when absent unreadable or not an object
func readOAuthHealCleanMarks(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))))
	dec.UseNumber()
	var data any
	if dec.Decode(&data) != nil {
		return nil
	}
	marks, _ := data.(map[string]any)
	return marks
}

// Stored clean mark fingerprint text for the provider, false when there is no usable mark
func persistedOAuthHealFingerprint(env PoolEnv, providerID string) (string, bool) {
	stored, ok := readOAuthHealCleanMarks(oauthHealCleanMarkPath(env))[providerID].([]any)
	if !ok {
		return "", false
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// Records the provider clean for this fingerprint on disk, a failed write only costs the next process a heal
func persistOAuthHealCleanMark(env PoolEnv, providerID string, fingerprint []any) {
	path := oauthHealCleanMarkPath(env)
	marks := map[string]any{}
	for key, value := range readOAuthHealCleanMarks(path) {
		if list, ok := value.([]any); ok {
			marks[key] = list
		}
	}
	if existing, ok := marks[providerID].([]any); ok {
		if raw, err := json.Marshal(existing); err == nil && string(raw) == oauthHealFingerprintJSON(fingerprint) {
			return
		}
	}
	marks[providerID] = fingerprint
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		var raw []byte
		if raw, err = marshalIndented(marks); err == nil {
			err = config.AtomicWrite(path, raw, 0o600)
		}
	}
	if err != nil {
		env.log().Debug(fmt.Sprintf("%s: could not persist the forked-OAuth clean mark: %v", providerID, err))
	}
}

func markOAuthHealClean(env PoolEnv, providerID string, fingerprint []any) {
	oauthHealMu.Lock()
	oauthHealCleanMarks[providerID] = oauthHealFingerprintJSON(fingerprint)
	oauthHealMu.Unlock()
	persistOAuthHealCleanMark(env, providerID, fingerprint)
}

// ConsumeOAuthHealNotices returns and clears the notes about forked grant heals run in this process
func ConsumeOAuthHealNotices() []string {
	oauthHealMu.Lock()
	defer oauthHealMu.Unlock()
	notes := append([]string{}, oauthHealNotices...)
	oauthHealNotices = nil
	return notes
}

func oauthFresher(candidate, target map[string]any) bool {
	order, ok := oauthPairOrder(candidate, target)
	return ok && order == 1
}

// Python equality of two decoded JSON values where booleans compare as integers
func pyJSONEqual(a, b any) bool {
	norm := func(v any) any {
		if t, ok := v.(bool); ok {
			if t {
				return float64(1)
			}
			return float64(0)
		}
		return v
	}
	a, b = norm(a), norm(b)
	switch at := a.(type) {
	case map[string]any:
		bt, ok := b.(map[string]any)
		if !ok || len(at) != len(bt) {
			return false
		}
		for k, v := range at {
			w, ok := bt[k]
			if !ok || !pyJSONEqual(v, w) {
				return false
			}
		}
		return true
	case []any:
		bt, ok := b.([]any)
		if !ok || len(at) != len(bt) {
			return false
		}
		for i := range at {
			if !pyJSONEqual(at[i], bt[i]) {
				return false
			}
		}
		return true
	case string, float64, nil:
		return a == b
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// Index of the root OAuth row sharing the profile row's lineage by copied id or token material, -1 when none
func findRootCounterpart(profileRow map[string]any, rootRows []any) int {
	var candidates []int
	for i, r := range rootRows {
		if isOAuthPoolPayload(r) {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return -1
	}
	pid := profileRow["id"]
	for _, i := range candidates {
		if truthyAny(pid) && pyJSONEqual(rootRows[i].(map[string]any)["id"], pid) {
			return i
		}
	}
	for _, key := range []string{"refresh_token", "access_token"} {
		pVal := profileRow[key]
		if !nonblank(pVal) {
			continue
		}
		for _, i := range candidates {
			if pyJSONEqual(rootRows[i].(map[string]any)[key], pVal) {
				return i
			}
		}
	}
	return -1
}

// Target carrying the winner's token pair with every pool status marker reset to null
func adoptOAuthMaterial(target, winner map[string]any) map[string]any {
	merged := cloneMap(target)
	for _, key := range oauthTokenFields {
		if v, ok := winner[key]; ok && v != nil {
			merged[key] = v
		} else {
			delete(merged, key)
		}
	}
	for _, f := range poolStatusFields {
		merged[f] = nil
	}
	return merged
}

// OAuthHealSummary reports one forked grant consolidation into the root store
type OAuthHealSummary struct {
	Adopted     bool
	StrippedIDs []any
}

// Mtime ns, size, inode and ctime ns from one stat, nil when the path is absent
func statSig(path string) any {
	if path == "" {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	ino, ctime := fileIdentity(path, st)
	return []int64{st.ModTime().UnixNano(), st.Size(), ino, ctime}
}

func storePoolRows(store AuthData, providerID string) (map[string]any, []any) {
	pool, _ := store["credential_pool"].(map[string]any)
	return pool, listOrEmpty(pool[providerID])
}

func adoptIfFresher(target, candidate map[string]any) map[string]any {
	if oauthFresher(candidate, target) {
		return adoptOAuthMaterial(target, candidate)
	}
	return nil
}

// One heal pass of a profile store against the root store for one provider
type oauthHealPass struct {
	provider       string
	summary        OAuthHealSummary
	profileChanged bool
	rootChanged    bool
	pPool          map[string]any
	pRows          []any
	rPool          map[string]any
	rRows          []any
}

func newOAuthHealPass(profileStore, rootStore AuthData, providerID string) *oauthHealPass {
	run := &oauthHealPass{provider: providerID, summary: OAuthHealSummary{StrippedIDs: []any{}}}
	run.pPool, run.pRows = storePoolRows(profileStore, providerID)
	run.rPool, run.rRows = storePoolRows(rootStore, providerID)
	return run
}

func (run *oauthHealPass) adoptRootRow(idx int, row map[string]any) {
	if merged := adoptIfFresher(run.rRows[idx].(map[string]any), row); merged != nil {
		run.rRows[idx] = merged
		run.rootChanged, run.summary.Adopted = true, true
	}
}

// Strips profile OAuth rows with a root counterpart after letting a fresher profile pair win into root
func (run *oauthHealPass) healPoolRows() {
	kept := []any{}
	for _, raw := range run.pRows {
		if !isOAuthPoolPayload(raw) {
			kept = append(kept, raw)
			continue
		}
		row := raw.(map[string]any)
		idx := findRootCounterpart(row, run.rRows)
		if idx < 0 {
			kept = append(kept, raw)
			continue
		}
		run.adoptRootRow(idx, row)
		run.summary.StrippedIDs = append(run.summary.StrippedIDs, row["id"])
		run.profileChanged = true
	}
	if run.profileChanged && run.pPool != nil {
		if len(kept) > 0 {
			run.pPool[run.provider] = kept
		} else {
			delete(run.pPool, run.provider)
		}
	}
}

func (run *oauthHealPass) dirty() bool {
	return run.profileChanged || run.rootChanged || run.summary.Adopted
}

func (run *oauthHealPass) notice(profileName string) string {
	var bits []string
	if len(run.summary.StrippedIDs) > 0 {
		ids := make([]string, len(run.summary.StrippedIDs))
		for i, id := range run.summary.StrippedIDs {
			ids[i] = pyRepr(id)
		}
		bits = append(bits, "pool rows ["+strings.Join(ids, ", ")+"]")
	}
	detail := strings.Join(bits, "; ")
	if detail == "" {
		detail = "no-op"
	}
	verdict := "root copy already newest; profile copy dropped"
	if run.summary.Adopted {
		verdict = "profile copy was the live pair; root updated"
	}
	return fmt.Sprintf("profile %s: consolidated forked %s OAuth grant (%s) into the root grant — %s; this profile now borrows the root grant (#100339)", profileName, run.provider, detail, verdict)
}

// Consolidates a single use grant forked into the active profile back into root, nil when nothing healed
func healForkedSingleUseOAuthGrants(env PoolEnv, provider string) *OAuthHealSummary {
	if !SingleUseRefreshPoolProviders[provider] {
		return nil
	}
	summary, err := runOAuthGrantHeal(env, provider)
	if err != nil {
		env.log().Debug(fmt.Sprintf("%s: forked-OAuth heal skipped: %v", provider, err))
		return nil
	}
	return summary
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func runOAuthGrantHeal(env PoolEnv, provider string) (*OAuthHealSummary, error) {
	rootPath := guardedGlobalRoot(env.Store.GlobalPath())
	if rootPath == "" {
		return nil, nil
	}
	profilePath := env.Store.Path()
	profileHome := filepath.Dir(profilePath)
	fingerprint := []any{profilePath, statSig(profilePath), rootPath, statSig(rootPath)}
	fpText := oauthHealFingerprintJSON(fingerprint)
	oauthHealMu.Lock()
	inMemory := oauthHealCleanMarks[provider] == fpText
	oauthHealMu.Unlock()
	if inMemory {
		return nil, nil
	}
	if persisted, ok := persistedOAuthHealFingerprint(env, provider); ok && persisted == fpText {
		oauthHealMu.Lock()
		oauthHealCleanMarks[provider] = fpText
		oauthHealMu.Unlock()
		return nil, nil
	}
	if fingerprint[1] == nil {
		markOAuthHealClean(env, provider, fingerprint)
		return nil, nil
	}
	if SameAuthStore(profilePath, rootPath) {
		markOAuthHealClean(env, provider, fingerprint)
		env.log().Debug(fmt.Sprintf("%s: forked-OAuth heal skipped, %s is the root store", provider, profilePath))
		return nil, nil
	}
	unlockProfile, err := env.Store.fileLock(profilePath, AuthLockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlockProfile()
	loadOrEmpty := func(path string) (AuthData, error) {
		if !pathExists(path) {
			return AuthData{"providers": map[string]any{}}, nil
		}
		return env.Store.LoadFile(path)
	}
	profileStore, err := loadOrEmpty(profilePath)
	if err != nil {
		return nil, err
	}
	unlockRoot, err := env.Store.fileLock(rootPath, AuthLockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlockRoot()
	rootStore, err := loadOrEmpty(rootPath)
	if err != nil {
		return nil, err
	}
	run := newOAuthHealPass(profileStore, rootStore, provider)
	run.healPoolRows()
	if !run.dirty() {
		markOAuthHealClean(env, provider, fingerprint)
		return nil, nil
	}
	if run.rootChanged {
		if run.rPool != nil {
			run.rPool[provider] = run.rRows
		} else {
			rootStore["credential_pool"] = map[string]any{provider: run.rRows}
		}
		if err := env.Store.SaveTo(rootStore, rootPath); err != nil {
			return nil, err
		}
	}
	if run.profileChanged && pathExists(profilePath) {
		if err := env.Store.SaveTo(profileStore, profilePath); err != nil {
			return nil, err
		}
	}
	message := run.notice(filepath.Base(profileHome))
	env.log().Info(message)
	oauthHealMu.Lock()
	oauthHealNotices = append(oauthHealNotices, message)
	oauthHealMu.Unlock()
	summary := run.summary
	return &summary, nil
}

// JWT iat, local issue stamp and expiry epoch seconds of an OAuth pair
func oauthStamps(entry map[string]any) (iat, local, expires float64, hasIat, hasLocal, hasExpires bool) {
	claims := DecodeJWTClaims(anyString(entry["access_token"]))
	first := func(values ...any) (float64, bool) {
		for _, v := range values {
			if ts, ok := ParseAbsoluteTimestamp(v); ok && ts != 0 {
				return ts, true
			}
		}
		return 0, false
	}
	iat, hasIat = first(claims["iat"])
	local, hasLocal = first(entry["last_refresh"], entry["obtained_at"])
	expires, hasExpires = first(claims["exp"], entry["expires_at_ms"], entry["expires_at"])
	return
}

// Order of two stamps when both are known
func oauthStampSign(x float64, hasX bool, y float64, hasY bool) (int, bool) {
	if !hasX || !hasY {
		return 0, false
	}
	switch {
	case x > y:
		return 1, true
	case x < y:
		return -1, true
	}
	return 0, true
}

// 1 when a holds the later issued OAuth pair than b, -1 the earlier, 0 tied, false when neither is stamped
func oauthPairOrder(a, b map[string]any) (int, bool) {
	ia, la, ea, hasIa, hasLa, hasEa := oauthStamps(a)
	ib, lb, eb, hasIb, hasLb, hasEb := oauthStamps(b)
	if hasIa && hasIb {
		if s, _ := oauthStampSign(ia, true, ib, true); s != 0 {
			return s, true
		}
		if s, ok := oauthStampSign(la, hasLa, lb, hasLb); ok && s != 0 {
			return s, true
		}
		return 0, true
	}
	issuedA, hasIssuedA := ia, hasIa
	if !hasIssuedA {
		issuedA, hasIssuedA = la, hasLa
	}
	issuedB, hasIssuedB := ib, hasIb
	if !hasIssuedB {
		issuedB, hasIssuedB = lb, hasLb
	}
	if s, ok := oauthStampSign(issuedA, hasIssuedA, issuedB, hasIssuedB); ok {
		return s, true
	}
	if s, ok := oauthStampSign(ea, hasEa, eb, hasEb); ok && s != 0 {
		return s, true
	}
	aAny := hasIa || hasLa || hasEa
	bAny := hasIb || hasLb || hasEb
	if !aAny && !bAny {
		return 0, false
	}
	if aAny == bAny {
		return 0, true
	}
	if aAny {
		return 1, true
	}
	return -1, true
}
