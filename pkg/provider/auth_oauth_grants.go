package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// SingleUseOAuthSingletonFiles hold single use grants outside auth.json and must never be copied into a profile
var SingleUseOAuthSingletonFiles = []string{".anthropic_oauth.json"}

var deviceCodeBlockProviders = []string{"openai-codex", "xai-oauth", "nous"}

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

// Codex and xAI nest the pair under tokens while Nous keeps it flat on the block
func blockTokens(block map[string]any) map[string]any {
	if tokens, ok := block["tokens"].(map[string]any); ok {
		return tokens
	}
	return block
}

// An OAuth row carrying a single use refresh grant, agent key only nous rows excluded
func isForkablePoolRow(providerID string, entry any) bool {
	if !isOAuthPoolPayload(entry) {
		return false
	}
	return providerID != "nous" || strings.TrimSpace(orEmptyStr(entry.(map[string]any)["refresh_token"])) != ""
}

// OAuth by auth_type, a refresh token or a legacy Anthropic OAuth access token
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
			if isForkablePoolRow(providerID, row) {
				liveGrants = append(liveGrants, row)
			}
		}
		snapshotHasGrants := false
		for _, row := range snapshotRows {
			if isForkablePoolRow(providerID, row) {
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
			liveIsGrant := liveRow != nil && isForkablePoolRow(providerID, liveRow)
			if isForkablePoolRow(providerID, snapshotRow) {
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
			if row := liveByID[id]; isForkablePoolRow(providerID, row) && !consumed[id] {
				merged = append(merged, deepCopyAny(row))
			}
		}
		snapshotPool[providerID] = append(merged, mergedSnapshot...)
	}
	snapshotProviders, _ := restored["providers"].(map[string]any)
	liveProviders, _ := live["providers"].(map[string]any)
	for _, providerID := range deviceCodeBlockProviders {
		snapshotBlock, snapOK := snapshotProviders[providerID].(map[string]any)
		liveBlock, liveOK := liveProviders[providerID].(map[string]any)
		snapshotHasRefresh := snapOK && truthyAny(blockTokens(snapshotBlock)["refresh_token"])
		liveHasRefresh := liveOK && truthyAny(blockTokens(liveBlock)["refresh_token"])
		if !snapshotHasRefresh && !liveHasRefresh {
			continue
		}
		if snapshotProviders == nil {
			snapshotProviders = map[string]any{}
			restored["providers"] = snapshotProviders
		}
		if liveOK {
			snapshotProviders[providerID] = deepCopyAny(liveBlock)
		} else {
			delete(snapshotProviders, providerID)
		}
	}
	return restored
}

func isPKCERow(row map[string]any) bool {
	return strings.HasSuffix(orEmptyStr(row["source"]), "hermes_pkce")
}

// ClonedGrantStrip names the pool providers, provider blocks and singleton files removed from a clone
type ClonedGrantStrip struct {
	Pool      []string
	Providers []string
	Files     []string
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

// StripClonedSingleUseOAuthGrants removes forked single use OAuth grants from a freshly cloned profile, keeping API key rows
func StripClonedSingleUseOAuthGrants(profileDir string) ClonedGrantStrip {
	stripped := ClonedGrantStrip{Pool: []string{}, Providers: []string{}, Files: []string{}}
	for _, name := range SingleUseOAuthSingletonFiles {
		target := filepath.Join(profileDir, name)
		if st, err := os.Lstat(target); err == nil && (st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0) {
			if err := os.Remove(target); err != nil {
				slog.Default().Debug(fmt.Sprintf("Could not remove cloned %s from %s: %v", name, profileDir, err))
			} else {
				stripped.Files = append(stripped.Files, name)
			}
		}
	}
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
	changed := false
	if pool, ok := store["credential_pool"].(map[string]any); ok {
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
				if !isForkablePoolRow(providerID, e) {
					kept = append(kept, e)
				}
			}
			if len(kept) != len(entries) {
				changed = true
				stripped.Pool = append(stripped.Pool, providerID)
				if len(kept) > 0 {
					pool[providerID] = kept
				} else {
					delete(pool, providerID)
				}
			}
		}
	}
	if providers, ok := store["providers"].(map[string]any); ok {
		for _, providerID := range deviceCodeBlockProviders {
			if block, ok := providers[providerID].(map[string]any); ok && truthyAny(blockTokens(block)["refresh_token"]) {
				delete(providers, providerID)
				stripped.Providers = append(stripped.Providers, providerID)
				changed = true
			}
		}
	}
	if !changed {
		return stripped
	}
	if err := NewStore(profileDir).SaveTo(AuthData(store), authPath); err != nil {
		slog.Default().Debug(fmt.Sprintf("Failed to strip cloned single-use OAuth grants from %s: %v", authPath, err))
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

// Stable account identity from JWT claims of the access or id token, empty when neither carries one
func oauthIdentity(entry map[string]any) string {
	if entry == nil {
		return ""
	}
	for _, token := range []any{entry["access_token"], entry["id_token"]} {
		claims := DecodeJWTClaims(anyString(token))
		if len(claims) == 0 {
			continue
		}
		var account any
		if nested, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
			account = nested["chatgpt_account_id"]
		}
		for _, value := range []any{account, claims["sub"], claims["email"]} {
			if nonblank(value) {
				return strings.TrimSpace(value.(string))
			}
		}
	}
	return ""
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

// Pool row shaped view of an Anthropic PKCE singleton file, nil when it holds no access token
func singletonAsRow(path string) map[string]any {
	data, ok := readJSONObjectFile(path)
	if !ok || strings.TrimSpace(orEmptyStr(data["accessToken"])) == "" {
		return nil
	}
	return map[string]any{"access_token": data["accessToken"], "refresh_token": data["refreshToken"], "expires_at_ms": data["expiresAt"]}
}

// OAuthHealSummary reports one forked grant consolidation into the root store
type OAuthHealSummary struct {
	Adopted        bool
	StrippedIDs    []any
	Files          []string
	ProvidersBlock bool
}

// Consolidates a forked providers block into root, matched false when no copy matched and adopted when the profile pair was fresher
func healForkedProviderBlock(profileStore, rootStore AuthData, providerID string, lineageProven bool) (adopted, matched bool) {
	pProviders, ok1 := profileStore["providers"].(map[string]any)
	rProviders, ok2 := rootStore["providers"].(map[string]any)
	if !ok1 || !ok2 {
		return false, false
	}
	pBlock, ok1 := pProviders[providerID].(map[string]any)
	rBlock, ok2 := rProviders[providerID].(map[string]any)
	if !ok1 || !truthyAny(blockTokens(pBlock)["refresh_token"]) || !ok2 || len(rBlock) == 0 {
		return false, false
	}
	flat := func(block map[string]any) map[string]any {
		out := cloneMap(blockTokens(block))
		out["last_refresh"] = block["last_refresh"]
		return out
	}
	pFlat, rFlat := flat(pBlock), flat(rBlock)
	if !lineageProven {
		shared := false
		for _, key := range []string{"access_token", "refresh_token"} {
			if truthyAny(pFlat[key]) && pyJSONEqual(pFlat[key], rFlat[key]) {
				shared = true
			}
		}
		if !shared {
			return false, false
		}
	}
	adopted = oauthFresher(pFlat, rFlat)
	if adopted {
		rProviders[providerID] = cloneMap(pBlock)
	}
	delete(pProviders, providerID)
	return adopted, true
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
	env              PoolEnv
	profileStore     AuthData
	rootStore        AuthData
	provider         string
	summary          OAuthHealSummary
	profileChanged   bool
	rootChanged      bool
	lineageProven    bool
	pPool            map[string]any
	pRows            []any
	rPool            map[string]any
	rRows            []any
	rOAuth           bool
	rootSingleton    string
	rootSingletonRow map[string]any
}

func newOAuthHealPass(env PoolEnv, profileStore, rootStore AuthData, providerID, rootSingleton string) *oauthHealPass {
	run := &oauthHealPass{env: env, profileStore: profileStore, rootStore: rootStore, provider: providerID, rootSingleton: rootSingleton}
	run.summary = OAuthHealSummary{StrippedIDs: []any{}, Files: []string{}}
	run.pPool, run.pRows = storePoolRows(profileStore, providerID)
	run.rPool, run.rRows = storePoolRows(rootStore, providerID)
	for _, r := range run.rRows {
		if isOAuthPoolPayload(r) {
			run.rOAuth = true
		}
	}
	if rootSingleton != "" {
		if _, err := os.Stat(rootSingleton); err == nil {
			run.rootSingletonRow = singletonAsRow(rootSingleton)
		}
	}
	return run
}

func (run *oauthHealPass) adoptRootRow(idx int, row map[string]any) {
	if merged := adoptIfFresher(run.rRows[idx].(map[string]any), row); merged != nil {
		run.rRows[idx] = merged
		run.rootChanged, run.summary.Adopted = true, true
	}
}

func (run *oauthHealPass) adoptRootSingleton(row map[string]any) {
	if merged := adoptIfFresher(run.rootSingletonRow, row); merged != nil {
		run.rootSingletonRow = merged
		run.summary.Adopted = true
	}
}

// Strips profile OAuth rows with a root counterpart after letting a fresher profile pair win into root
func (run *oauthHealPass) healPoolRows() {
	kept := []any{}
	for _, raw := range run.pRows {
		if !isForkablePoolRow(run.provider, raw) {
			kept = append(kept, raw)
			continue
		}
		row := raw.(map[string]any)
		if idx := findRootCounterpart(row, run.rRows); idx >= 0 {
			run.lineageProven = true
			run.adoptRootRow(idx, row)
		} else if row["source"] == "hermes_pkce" && run.rootSingletonRow != nil && !run.rOAuth {
			run.adoptRootSingleton(row)
		} else {
			kept = append(kept, raw)
			continue
		}
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

func (run *oauthHealPass) healProviderBlock() {
	isBlockProvider := false
	for _, p := range deviceCodeBlockProviders {
		isBlockProvider = isBlockProvider || p == run.provider
	}
	if !isBlockProvider {
		return
	}
	adopted, matched := healForkedProviderBlock(run.profileStore, run.rootStore, run.provider, run.lineageProven)
	if matched {
		run.profileChanged, run.summary.ProvidersBlock = true, true
		if adopted {
			run.rootChanged, run.summary.Adopted = true, true
		}
	}
}

// Folds a profile PKCE singleton into root's grant and unlinks it, never touching an aliased or only surviving copy
func (run *oauthHealPass) healProfileSingleton(profileSingleton string) {
	if profileSingleton == "" {
		return
	}
	if _, err := os.Stat(profileSingleton); err != nil {
		return
	}
	if run.rootSingleton != "" && SameAuthStore(profileSingleton, run.rootSingleton) {
		return
	}
	pSingle := singletonAsRow(profileSingleton)
	rootHasGrant := run.rOAuth || run.rootSingletonRow != nil
	if pSingle == nil || !rootHasGrant {
		return
	}
	if run.rootSingletonRow != nil {
		run.adoptRootSingleton(pSingle)
	} else {
		for i, r := range run.rRows {
			if isOAuthPoolPayload(r) && isPKCERow(r.(map[string]any)) {
				run.adoptRootRow(i, pSingle)
				break
			}
		}
	}
	if err := os.Remove(profileSingleton); err != nil {
		run.env.log().Debug(fmt.Sprintf("could not remove %s: %v", profileSingleton, err))
		return
	}
	run.summary.Files = append(run.summary.Files, filepath.Base(profileSingleton))
}

// Keeps root's PKCE singleton and its hermes_pkce seeded pool row on the same freshest pair
func (run *oauthHealPass) syncRootSingletonWithPKCERow() {
	if !run.summary.Adopted || run.rootSingleton == "" || run.rootSingletonRow == nil {
		return
	}
	pkceIdx := -1
	for i, r := range run.rRows {
		if isOAuthPoolPayload(r) && r.(map[string]any)["source"] == "hermes_pkce" {
			pkceIdx = i
			break
		}
	}
	if pkceIdx < 0 {
		return
	}
	pkceRow := run.rRows[pkceIdx].(map[string]any)
	order, ok := oauthPairOrder(pkceRow, run.rootSingletonRow)
	switch {
	case ok && order == 1:
		run.rootSingletonRow = adoptOAuthMaterial(run.rootSingletonRow, pkceRow)
	case ok && order == -1:
		run.rRows[pkceIdx] = adoptOAuthMaterial(pkceRow, run.rootSingletonRow)
		run.rootChanged = true
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
	if run.summary.ProvidersBlock {
		bits = append(bits, "providers."+run.provider+" block")
	}
	if len(run.summary.Files) > 0 {
		bits = append(bits, strings.Join(run.summary.Files, ", "))
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
	var profileSingleton, rootSingleton string
	if provider == "anthropic" {
		profileSingleton = filepath.Join(profileHome, ".anthropic_oauth.json")
		rootSingleton = filepath.Join(filepath.Dir(rootPath), ".anthropic_oauth.json")
	}
	fingerprint := []any{profilePath, statSig(profilePath), statSig(profileSingleton), rootPath, statSig(rootPath), statSig(rootSingleton)}
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
	if fingerprint[1] == nil && fingerprint[2] == nil {
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
	run := newOAuthHealPass(env, profileStore, rootStore, provider, rootSingleton)
	run.healPoolRows()
	run.healProviderBlock()
	run.healProfileSingleton(profileSingleton)
	if !run.dirty() {
		markOAuthHealClean(env, provider, fingerprint)
		return nil, nil
	}
	run.syncRootSingletonWithPKCERow()
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
	if row := run.rootSingletonRow; run.summary.Adopted && rootSingleton != "" && row != nil {
		payload := map[string]any{"accessToken": orEmptyStr(row["access_token"]), "refreshToken": row["refresh_token"], "expiresAt": row["expires_at_ms"]}
		if err := commitPrivateJSON(env, rootSingleton, payload, "Icebath OAuth credentials"); err != nil {
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
