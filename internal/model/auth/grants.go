package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

var singleUseRefreshPoolProviders = []string{ProviderAnthropic, ProviderCodex, ProviderXAIOAuth, ProviderPortal}

var deviceCodeBlockProviders = []string{ProviderCodex, ProviderXAIOAuth, ProviderPortal}

var oauthTokenFields = []string{"access_token", "refresh_token", "expires_at", "expires_at_ms", "last_refresh"}

const sourceIcebathPKCE = "icebath_pkce"

func IsSingleUseRefreshProvider(provider string) bool {
	return slices.Contains(singleUseRefreshPoolProviders, provider)
}

func blockTokens(block *doc) *doc {
	if tokens := block.obj("tokens"); tokens != nil {
		return tokens
	}
	return block
}

func isOAuthPoolPayload(entry *doc) bool {
	if entry == nil {
		return false
	}
	return strings.ToLower(strings.TrimSpace(textOf(entry.value("auth_type")))) == AuthTypeOAuth ||
		strings.TrimSpace(textOf(entry.value("refresh_token"))) != "" ||
		strings.HasPrefix(textOf(entry.value("access_token")), "sk-ant-oat")
}

func isForkablePoolRow(provider string, entry *doc) bool {
	if !isOAuthPoolPayload(entry) {
		return false
	}
	return provider != ProviderPortal || strings.TrimSpace(textOf(entry.value("refresh_token"))) != ""
}

func asRow(v any) *doc {
	row, _ := v.(*doc)
	return row
}

func stringID(row *doc) string {
	id, _ := row.value("id").(string)
	return id
}

func mergeSnapshotPreservingLiveGrants(snapshot, live *doc) *doc {
	restored := deepCopy(snapshot).(*doc)
	snapshotPool := restored.obj("credential_pool")
	livePool := live.obj("credential_pool")
	for _, provider := range singleUseRefreshPoolProviders {
		liveRows := livePool.list(provider)
		snapshotRows := snapshotPool.list(provider)
		liveGrants := []*doc{}
		for _, v := range liveRows {
			if row := asRow(v); isForkablePoolRow(provider, row) {
				liveGrants = append(liveGrants, row)
			}
		}
		snapshotHasGrants := slices.ContainsFunc(snapshotRows, func(v any) bool { return isForkablePoolRow(provider, asRow(v)) })
		if len(liveGrants) == 0 && !snapshotHasGrants {
			continue
		}
		if snapshotPool == nil {
			snapshotPool = newDoc()
			restored.set("credential_pool", snapshotPool)
		}
		liveByID := map[string]*doc{}
		liveOrder := []string{}
		for _, v := range liveRows {
			row := asRow(v)
			if id := stringID(row); row != nil && id != "" {
				if _, seen := liveByID[id]; !seen {
					liveOrder = append(liveOrder, id)
				}
				liveByID[id] = row
			}
		}
		merged := []any{}
		for _, row := range liveGrants {
			if stringID(row) == "" {
				merged = append(merged, deepCopy(row))
			}
		}
		consumed := map[string]bool{}
		restoredRows := []any{}
		for _, v := range snapshotRows {
			snapshotRow := asRow(v)
			rowID := stringID(snapshotRow)
			var liveRow *doc
			if rowID != "" {
				liveRow = liveByID[rowID]
			}
			liveIsGrant := isForkablePoolRow(provider, liveRow)
			if isForkablePoolRow(provider, snapshotRow) {
				if liveRow == nil || consumed[rowID] {
					continue
				}
				consumed[rowID] = true
				if liveIsGrant {
					base := credentialTokenPair(snapshotRow)
					restoredRows = append(restoredRows, mergePoolRowGeneration(snapshotRow, liveRow, provider, &base, false))
				} else {
					restoredRows = append(restoredRows, deepCopy(liveRow))
				}
				continue
			}
			if liveIsGrant {
				if consumed[rowID] {
					continue
				}
				consumed[rowID] = true
				restoredRows = append(restoredRows, deepCopy(liveRow))
			} else {
				restoredRows = append(restoredRows, v)
			}
		}
		for _, id := range liveOrder {
			if row := liveByID[id]; isForkablePoolRow(provider, row) && !consumed[id] {
				merged = append(merged, deepCopy(row))
			}
		}
		snapshotPool.set(provider, append(merged, restoredRows...))
	}
	snapshotProviders := restored.obj("providers")
	liveProviders := live.obj("providers")
	for _, provider := range deviceCodeBlockProviders {
		snapshotBlock := snapshotProviders.obj(provider)
		liveBlock := liveProviders.obj(provider)
		snapshotHasRefresh := snapshotBlock != nil && truthy(blockTokens(snapshotBlock).value("refresh_token"))
		liveHasRefresh := liveBlock != nil && truthy(blockTokens(liveBlock).value("refresh_token"))
		if !snapshotHasRefresh && !liveHasRefresh {
			continue
		}
		if snapshotProviders == nil {
			snapshotProviders = newDoc()
			restored.set("providers", snapshotProviders)
		}
		if liveBlock != nil {
			snapshotProviders.set(provider, deepCopy(liveBlock))
		} else {
			snapshotProviders.del(provider)
		}
	}
	return restored
}

func MergeSnapshotPreservingLiveGrants(snapshot, live json.RawMessage) (json.RawMessage, error) {
	snap, err := parseObject(snapshot)
	if err != nil {
		return nil, err
	}
	current, err := parseObject(live)
	if err != nil {
		return nil, err
	}
	return mergeSnapshotPreservingLiveGrants(snap, current).MarshalJSON()
}

func isPKCERow(row *doc) bool {
	return strings.HasSuffix(textOf(row.value("source")), sourceIcebathPKCE)
}

type GrantStrip struct {
	Pool      []string
	Providers []string
	Files     []string
}

func (s *Store) StripClonedSingleUseGrants(ctx context.Context, profileDir string) GrantStrip {
	stripped := GrantStrip{}
	name := string(home.AnthropicOAuth)
	target := home.AnthropicOAuth.In(profileDir)
	if fi, err := os.Lstat(target); err == nil {
		regular := fi.Mode().IsRegular()
		if fi.Mode()&os.ModeSymlink != 0 {
			regular = true
		}
		if regular {
			if err := os.Remove(target); err != nil {
				slog.Debug(fmt.Sprintf("Could not remove cloned %s from %s", name, profileDir), "error", err)
			} else {
				stripped.Files = append(stripped.Files, name)
			}
		}
	}
	authPath := home.AuthFile.In(profileDir)
	fi, err := os.Stat(authPath)
	if err != nil || !fi.Mode().IsRegular() {
		return stripped
	}
	rootAuth := home.AuthFile.In(s.root)
	if samePath(authPath, rootAuth) {
		return stripped
	}
	rootInfo, err := os.Stat(rootAuth)
	switch {
	case err == nil:
		if os.SameFile(fi, rootInfo) {
			return stripped
		}
	case !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR):
		return stripped
	}
	data, err := os.ReadFile(authPath)
	if err != nil {
		return stripped
	}
	store, err := parseObject(data)
	if err != nil {
		return stripped
	}
	changed := false
	if pool := store.obj("credential_pool"); pool != nil {
		for _, provider := range slices.Clone(pool.keys) {
			entries, isList := pool.value(provider).([]any)
			if !slices.Contains(singleUseRefreshPoolProviders, provider) || !isList {
				continue
			}
			kept := []any{}
			for _, v := range entries {
				if !isForkablePoolRow(provider, asRow(v)) {
					kept = append(kept, v)
				}
			}
			if len(kept) == len(entries) {
				continue
			}
			changed = true
			stripped.Pool = append(stripped.Pool, provider)
			if len(kept) > 0 {
				pool.set(provider, kept)
			} else {
				pool.del(provider)
			}
		}
	}
	if providers := store.obj("providers"); providers != nil {
		for _, provider := range deviceCodeBlockProviders {
			if block := providers.obj(provider); block != nil && truthy(blockTokens(block).value("refresh_token")) {
				providers.del(provider)
				stripped.Providers = append(stripped.Providers, provider)
				changed = true
			}
		}
	}
	if !changed {
		return stripped
	}
	if err := s.save(ctx, store, authPath); err != nil {
		slog.Debug(fmt.Sprintf("Failed to strip cloned single-use OAuth grants from %s", authPath), "error", err)
	}
	return stripped
}

func (s *Store) ConsumeHealNotices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	notes := s.healNotices
	s.healNotices = nil
	return notes
}

func oauthIdentity(entry *doc) string {
	if entry == nil {
		return ""
	}
	for _, key := range []string{"access_token", "id_token"} {
		claims := decodeJWTClaims(entry.str(key))
		if claims.size() == 0 {
			continue
		}
		account := claims.obj("https://api.openai.com/auth").str("chatgpt_account_id")
		for _, value := range []string{account, claims.str("sub"), claims.str("email")} {
			if strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

type stamp struct {
	at    float64
	known bool
}

func firstStamp(values ...any) stamp {
	for _, v := range values {
		if ts, ok := parseAbsolute(v); ok && ts != 0 {
			return stamp{ts, true}
		}
	}
	return stamp{}
}

func oauthStamps(entry *doc) (iat, local, expires stamp) {
	claims := decodeJWTClaims(entry.str("access_token"))
	return firstStamp(claims.value("iat")),
		firstStamp(entry.value("last_refresh"), entry.value("obtained_at")),
		firstStamp(claims.value("exp"), entry.value("expires_at_ms"), entry.value("expires_at"))
}

func signOf(x, y stamp) (int, bool) {
	if !x.known || !y.known {
		return 0, false
	}
	switch {
	case x.at > y.at:
		return 1, true
	case x.at < y.at:
		return -1, true
	}
	return 0, true
}

func oauthPairOrder(a, b *doc) (int, bool) {
	ia, la, ea := oauthStamps(a)
	ib, lb, eb := oauthStamps(b)
	if ia.known && ib.known {
		if sign, _ := signOf(ia, ib); sign != 0 {
			return sign, true
		}
		if sign, ok := signOf(la, lb); ok && sign != 0 {
			return sign, true
		}
		return 0, true
	}
	issuedA, issuedB := ia, ib
	if !issuedA.known {
		issuedA = la
	}
	if !issuedB.known {
		issuedB = lb
	}
	if sign, ok := signOf(issuedA, issuedB); ok {
		return sign, true
	}
	if sign, ok := signOf(ea, eb); ok && sign != 0 {
		return sign, true
	}
	aAny := ia.known || la.known || ea.known
	bAny := ib.known || lb.known || eb.known
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

func oauthFresher(candidate, target *doc) bool {
	order, ok := oauthPairOrder(candidate, target)
	return ok && order == 1
}

func findRootCounterpart(profileRow *doc, rootRows []any) int {
	candidates := []int{}
	for i, v := range rootRows {
		if isOAuthPoolPayload(asRow(v)) {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return -1
	}
	if pid := profileRow.value("id"); truthy(pid) {
		for _, i := range candidates {
			if valueEqual(asRow(rootRows[i]).value("id"), pid) {
				return i
			}
		}
	}
	for _, key := range []string{"refresh_token", "access_token"} {
		value := profileRow.str(key)
		if strings.TrimSpace(value) == "" {
			continue
		}
		for _, i := range candidates {
			if asRow(rootRows[i]).str(key) == value {
				return i
			}
		}
	}
	return -1
}

func adoptOAuthMaterial(target, winner *doc) *doc {
	merged := target.clone()
	for _, key := range oauthTokenFields {
		if v := winner.value(key); v != nil {
			merged.set(key, v)
		} else {
			merged.del(key)
		}
	}
	for _, f := range poolStatusFields {
		merged.set(f, nil)
	}
	return merged
}

func singletonAsRow(path string) *doc {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	parsed, err := parseObject(data)
	if err != nil || strings.TrimSpace(textOf(parsed.value("accessToken"))) == "" {
		return nil
	}
	row := newDoc()
	row.set("access_token", parsed.value("accessToken"))
	row.set("refresh_token", parsed.value("refreshToken"))
	row.set("expires_at_ms", parsed.value("expiresAt"))
	return row
}

type HealSummary struct {
	Adopted        bool
	StrippedIDs    []string
	Files          []string
	ProvidersBlock bool
}

func healForkedProviderBlock(profileStore, rootStore *doc, provider string, lineageProven bool) (adopted, matched bool) {
	pProviders, rProviders := profileStore.obj("providers"), rootStore.obj("providers")
	if pProviders == nil || rProviders == nil {
		return false, false
	}
	pBlock, rBlock := pProviders.obj(provider), rProviders.obj(provider)
	if pBlock == nil || !truthy(blockTokens(pBlock).value("refresh_token")) || rBlock.size() == 0 {
		return false, false
	}
	flat := func(block *doc) *doc {
		out := blockTokens(block).clone()
		out.set("last_refresh", block.value("last_refresh"))
		return out
	}
	pFlat, rFlat := flat(pBlock), flat(rBlock)
	if !lineageProven {
		shared := false
		for _, key := range []string{"access_token", "refresh_token"} {
			if v := pFlat.value(key); truthy(v) && valueEqual(v, rFlat.value(key)) {
				shared = true
			}
		}
		if !shared {
			return false, false
		}
	}
	adopted = oauthFresher(pFlat, rFlat)
	if adopted {
		rProviders.set(provider, pBlock.clone())
	}
	pProviders.del(provider)
	return adopted, true
}

func (s *Store) ownedProfileReadsRootState(store *doc, provider, sourcePath string) bool {
	return sourcePath != "" && s.rootAuth != "" && samePath(sourcePath, s.rootAuth) &&
		len(store.obj("credential_pool").list(provider)) > 0
}

type healPass struct {
	profileStore, rootStore *doc
	provider                string
	summary                 HealSummary
	profileChanged          bool
	rootChanged             bool
	lineageProven           bool
	pPool, rPool            *doc
	pRows, rRows            []any
	rootHasOAuth            bool
	rootSingleton           string
	rootSingletonRow        *doc
}

func newHealPass(profileStore, rootStore *doc, provider, rootSingleton string) *healPass {
	h := &healPass{profileStore: profileStore, rootStore: rootStore, provider: provider, rootSingleton: rootSingleton}
	h.pPool, h.rPool = profileStore.obj("credential_pool"), rootStore.obj("credential_pool")
	h.pRows, h.rRows = h.pPool.list(provider), slices.Clone(h.rPool.list(provider))
	h.rootHasOAuth = slices.ContainsFunc(h.rRows, func(v any) bool { return isOAuthPoolPayload(asRow(v)) })
	if rootSingleton != "" {
		if _, err := os.Stat(rootSingleton); err == nil {
			h.rootSingletonRow = singletonAsRow(rootSingleton)
		}
	}
	return h
}

func (h *healPass) adoptRootRow(idx int, row *doc) {
	target := asRow(h.rRows[idx])
	if oauthFresher(row, target) {
		h.rRows[idx] = adoptOAuthMaterial(target, row)
		h.rootChanged, h.summary.Adopted = true, true
	}
}

func (h *healPass) adoptRootSingleton(row *doc) {
	if oauthFresher(row, h.rootSingletonRow) {
		h.rootSingletonRow = adoptOAuthMaterial(h.rootSingletonRow, row)
		h.summary.Adopted = true
	}
}

func (h *healPass) healPoolRows() {
	kept := []any{}
	for _, v := range h.pRows {
		row := asRow(v)
		if !isForkablePoolRow(h.provider, row) {
			kept = append(kept, v)
			continue
		}
		if idx := findRootCounterpart(row, h.rRows); idx >= 0 {
			h.lineageProven = true
			h.adoptRootRow(idx, row)
		} else if row.str("source") == sourceIcebathPKCE && h.rootSingletonRow != nil && !h.rootHasOAuth {
			h.adoptRootSingleton(row)
		} else {
			kept = append(kept, v)
			continue
		}
		h.summary.StrippedIDs = append(h.summary.StrippedIDs, textOf(row.value("id")))
		h.profileChanged = true
	}
	if h.profileChanged && h.pPool != nil {
		if len(kept) > 0 {
			h.pPool.set(h.provider, kept)
		} else {
			h.pPool.del(h.provider)
		}
	}
}

func (h *healPass) healProviderBlock() {
	if !slices.Contains(deviceCodeBlockProviders, h.provider) {
		return
	}
	adopted, matched := healForkedProviderBlock(h.profileStore, h.rootStore, h.provider, h.lineageProven)
	if !matched {
		return
	}
	h.profileChanged, h.summary.ProvidersBlock = true, true
	if adopted {
		h.rootChanged, h.summary.Adopted = true, true
	}
}

func (h *healPass) healProfileSingleton(profileSingleton string) {
	if profileSingleton == "" {
		return
	}
	if _, err := os.Stat(profileSingleton); err != nil {
		return
	}
	if h.rootSingleton != "" && sameStore(profileSingleton, h.rootSingleton) {
		return
	}
	pSingle := singletonAsRow(profileSingleton)
	rootHasGrant := h.rootHasOAuth || h.rootSingletonRow != nil
	if pSingle == nil || !rootHasGrant {
		return
	}
	if h.rootSingletonRow != nil {
		h.adoptRootSingleton(pSingle)
	} else {
		for i, v := range h.rRows {
			if row := asRow(v); isOAuthPoolPayload(row) && isPKCERow(row) {
				h.adoptRootRow(i, pSingle)
				break
			}
		}
	}
	if err := os.Remove(profileSingleton); err != nil {
		slog.Debug(fmt.Sprintf("could not remove %s", profileSingleton), "error", err)
		return
	}
	h.summary.Files = append(h.summary.Files, filepath.Base(profileSingleton))
}

func (h *healPass) syncRootSingletonWithPKCERow() {
	if !h.summary.Adopted || h.rootSingleton == "" || h.rootSingletonRow == nil {
		return
	}
	for i, v := range h.rRows {
		row := asRow(v)
		if !isOAuthPoolPayload(row) || row.str("source") != sourceIcebathPKCE {
			continue
		}
		order, ok := oauthPairOrder(row, h.rootSingletonRow)
		if ok && order == 1 {
			h.rootSingletonRow = adoptOAuthMaterial(h.rootSingletonRow, row)
		} else if ok && order == -1 {
			h.rRows[i] = adoptOAuthMaterial(row, h.rootSingletonRow)
			h.rootChanged = true
		}
		return
	}
}

func (h *healPass) dirty() bool { return h.profileChanged || h.rootChanged || h.summary.Adopted }

func (h *healPass) notice(profileName string) string {
	bits := []string{}
	if len(h.summary.StrippedIDs) > 0 {
		quoted := make([]string, 0, len(h.summary.StrippedIDs))
		for _, id := range h.summary.StrippedIDs {
			quoted = append(quoted, "'"+id+"'")
		}
		bits = append(bits, "pool rows ["+strings.Join(quoted, ", ")+"]")
	}
	if h.summary.ProvidersBlock {
		bits = append(bits, "providers."+h.provider+" block")
	}
	if len(h.summary.Files) > 0 {
		bits = append(bits, strings.Join(h.summary.Files, ", "))
	}
	detail := strings.Join(bits, "; ")
	if detail == "" {
		detail = "no-op"
	}
	verdict := "root copy already newest; profile copy dropped"
	if h.summary.Adopted {
		verdict = "profile copy was the live pair; root updated"
	}
	return fmt.Sprintf("profile %s: consolidated forked %s OAuth grant (%s) into the root grant — %s; this profile now borrows the root grant",
		profileName, h.provider, detail, verdict)
}

func (s *Store) persistedHealFingerprint(provider string) ([]any, bool) {
	data, err := os.ReadFile(home.OAuthHealClean.In(s.home))
	if err != nil {
		return nil, false
	}
	marks, err := parseObject(data)
	if err != nil {
		return nil, false
	}
	stored, ok := marks.value(provider).([]any)
	return stored, ok
}

func (s *Store) persistHealCleanMark(ctx context.Context, provider string, fingerprint []any) {
	path := home.OAuthHealClean.In(s.home)
	marks := newDoc()
	if data, err := os.ReadFile(path); err == nil {
		if existing, err := parseObject(data); err == nil {
			for _, k := range existing.keys {
				if list, ok := existing.vals[k].([]any); ok {
					marks.set(k, list)
				}
			}
		}
	}
	if valueEqual(marks.value(provider), fingerprint) {
		return
	}
	marks.set(provider, fingerprint)
	if err := s.homes.Mkdir(ctx, filepath.Dir(path)); err != nil {
		slog.Debug(fmt.Sprintf("%s: could not persist the forked-OAuth clean mark", provider), "error", err)
		return
	}
	if err := fileutil.WriteJSON(ctx, path, marks, fileutil.JSONStyle{Indent: 2}, fileutil.Private(), fileutil.TempPrefix(".oauth_heal_clean_")); err != nil {
		slog.Debug(fmt.Sprintf("%s: could not persist the forked-OAuth clean mark", provider), "error", err)
	}
}

func fingerprintKey(fingerprint []any) string {
	raw, err := json.Marshal(jsonView{fingerprint})
	if err != nil {
		return ""
	}
	return string(raw)
}

func (s *Store) markHealClean(ctx context.Context, provider string, fingerprint []any) {
	s.mu.Lock()
	s.healMarks[provider] = fingerprintKey(fingerprint)
	s.mu.Unlock()
	s.persistHealCleanMark(ctx, provider, fingerprint)
}

func (s *Store) HealForkedSingleUseGrants(ctx context.Context, provider string) *HealSummary {
	if !slices.Contains(singleUseRefreshPoolProviders, provider) {
		return nil
	}
	summary, err := s.healForkedGrants(s.bind(ctx), provider)
	if err != nil {
		slog.Debug(fmt.Sprintf("%s: forked-OAuth heal did not run", provider), "error", err)
		return nil
	}
	return summary
}

func (s *Store) healForkedGrants(ctx context.Context, provider string) (*HealSummary, error) {
	if s.rootAuth == "" {
		return nil, nil
	}
	profilePath, rootPath := s.authPath, s.rootAuth
	profileSingleton, rootSingleton := "", ""
	if provider == ProviderAnthropic {
		profileSingleton = home.AnthropicOAuth.In(s.home)
		rootSingleton = home.AnthropicOAuth.In(s.root)
	}
	profileSig, singletonSig := signatureValue(profilePath), signatureValue(profileSingleton)
	fingerprint := []any{profilePath, profileSig, singletonSig, rootPath, signatureValue(rootPath), signatureValue(rootSingleton)}
	key := fingerprintKey(fingerprint)
	s.mu.Lock()
	clean := s.healMarks[provider] == key
	s.mu.Unlock()
	if clean {
		return nil, nil
	}
	if stored, ok := s.persistedHealFingerprint(provider); ok && valueEqual(stored, fingerprint) {
		s.mu.Lock()
		s.healMarks[provider] = key
		s.mu.Unlock()
		return nil, nil
	}
	if profileSig == nil && singletonSig == nil {
		s.markHealClean(ctx, provider, fingerprint)
		return nil, nil
	}
	if sameStore(profilePath, rootPath) {
		s.markHealClean(ctx, provider, fingerprint)
		slog.Debug(fmt.Sprintf("%s: forked-OAuth heal not needed, %s is the root store", provider, profilePath))
		return nil, nil
	}
	ctx, unlock, err := s.lock(ctx, profilePath, LockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlock()
	profileStore, err := s.load(profilePath)
	if err != nil {
		return nil, err
	}
	inner, unlockRoot, err := s.lock(ctx, rootPath, LockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlockRoot()
	rootStore, err := s.load(rootPath)
	if err != nil {
		return nil, err
	}
	run := newHealPass(profileStore, rootStore, provider, rootSingleton)
	run.healPoolRows()
	run.healProviderBlock()
	run.healProfileSingleton(profileSingleton)
	if !run.dirty() {
		s.markHealClean(inner, provider, fingerprint)
		return nil, nil
	}
	run.syncRootSingletonWithPKCERow()
	if run.rootChanged {
		if run.rPool != nil {
			run.rPool.set(provider, run.rRows)
		} else {
			pool := newDoc()
			pool.set(provider, run.rRows)
			rootStore.set("credential_pool", pool)
		}
		if err := s.save(inner, rootStore, rootPath); err != nil {
			return nil, err
		}
	}
	if row := run.rootSingletonRow; run.summary.Adopted && rootSingleton != "" && row != nil {
		access := row.value("access_token")
		if !truthy(access) {
			access = ""
		}
		if err := s.writeAnthropicOAuthFile(inner, access, row.value("refresh_token"), row.value("expires_at_ms"), rootSingleton); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(profilePath); run.profileChanged && err == nil {
		if err := s.save(inner, profileStore, profilePath); err != nil {
			return nil, err
		}
	}
	message := run.notice(filepath.Base(s.home))
	slog.Info(message)
	s.mu.Lock()
	s.healNotices = append(s.healNotices, message)
	s.mu.Unlock()
	summary := run.summary
	return &summary, nil
}
