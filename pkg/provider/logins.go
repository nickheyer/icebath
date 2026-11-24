package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Pool row keys of a named login
const (
	loginNameKey    = "name"
	loginAccountKey = "account"
	loginRotateKey  = "rotate"
)

// Origins of a login: Icebath holds the secret, or the login points at an outside source
const (
	LoginOriginOwned  = "owned"
	LoginOriginLinked = "linked"
)

// States a login lists with
const (
	LoginStatusReady       = "ready"
	LoginStatusLimited     = "limited"
	LoginStatusNeedsSignIn = "needs_sign_in"
	LoginStatusDead        = "dead"
	LoginStatusUnavailable = "unavailable"
)

// Login metadata a merge always takes from the disk row
var loginMetaKeys = []string{loginNameKey, loginAccountKey, loginRotateKey}

var loginNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

var loginSlugRE = regexp.MustCompile(`[^a-z0-9]+`)

// LoginName is the row's login name, the stored slug else one derived from its source and label
func (e PooledCredential) LoginName() string {
	if name := strings.TrimSpace(e.ExtraString(loginNameKey)); name != "" {
		return name
	}
	return derivedLoginName(e)
}

// LoginRotate reports a login the user put in the provider's rotation set
func (e PooledCredential) LoginRotate() bool { return truthyAny(e.Extra[loginRotateKey]) }

// LoginAccount is the non secret account identity of the row, the stored one else one read from token claims
func (e PooledCredential) LoginAccount() string {
	if account := strings.TrimSpace(e.ExtraString(loginAccountKey)); account != "" {
		return account
	}
	return loginAccountFromToken(e.Provider, e.AccessToken)
}

// LoginOrigin is owned when Icebath holds the secret and linked when the row points at an outside source
func (e PooledCredential) LoginOrigin() string {
	if IsBorrowedCredentialSource(e.Source) {
		return LoginOriginLinked
	}
	return LoginOriginOwned
}

// Kinds of a refused login request
const (
	LoginFaultUnknown = "unknown_login"
	LoginFaultName    = "bad_name"
	LoginFaultValue   = "bad_value"
	// The provider signs in another way than the one asked
	LoginFaultAuthType = "wrong_auth_type"
	// An answer for a sign in that waits on no prompt
	LoginFaultNotWaiting = "not_waiting"
)

// LoginFault is a login request refused for a reason the user can fix, its kind naming which
type LoginFault struct {
	Kind string
	Err  error
}

func (f *LoginFault) Error() string { return f.Err.Error() }

func (f *LoginFault) Unwrap() error { return f.Err }

// Refusal of one kind with its message
func loginFault(kind, format string, args ...any) error {
	return &LoginFault{Kind: kind, Err: fmt.Errorf(format, args...)}
}

// SlugLoginName lowers text into a login name slug, empty when nothing usable remains
func SlugLoginName(text string) string {
	slug := strings.Trim(loginSlugRE.ReplaceAllString(strings.ToLower(strings.TrimSpace(text)), "-"), "-")
	if len(slug) > 40 {
		slug = strings.TrimRight(slug[:40], "-")
	}
	return slug
}

// ValidateLoginName refuses a name that is not a lowercase slug of at most 40 characters
func ValidateLoginName(name string) error {
	if !loginNameRE.MatchString(name) {
		return loginFault(LoginFaultName, "login name %q must be lowercase letters, digits and dashes, starting with a letter or digit, at most 40 characters", name)
	}
	return nil
}

// Name a row without a stored one goes by, from its env var, its external source or its label
func derivedLoginName(e PooledCredential) string {
	source := strings.TrimSpace(e.Source)
	switch {
	case strings.HasPrefix(source, "env:"):
		if slug := SlugLoginName(source[len("env:"):]); slug != "" {
			return slug
		}
	case source == "claude_code":
		return "claude-code"
	case source == "qwen-cli":
		return "qwen-cli"
	case strings.HasPrefix(source, "config:"), source == "model_config":
		if slug := SlugLoginName(strings.TrimPrefix(source, "config:")); slug != "" {
			return slug
		}
	}
	if slug := SlugLoginName(e.Label); slug != "" && e.Label != e.Source {
		return slug
	}
	if slug := SlugLoginName(strings.TrimPrefix(source, SourceManual+":")); slug != "" && source != SourceManual {
		return slug
	}
	return "login"
}

// First free name for base, suffixed -2, -3 when taken
func uniqueLoginName(base string, taken map[string]bool) string {
	if base == "" {
		base = "login"
	}
	if !taken[base] {
		return base
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", base, n)
		if !taken[candidate] {
			return candidate
		}
	}
}

// Account identity a token's claims name, the email first, then the Codex account id
func loginAccountFromToken(provider, token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if email := LabelFromToken(token, ""); email != "" {
		return email
	}
	if provider == "openai-codex" {
		if account, _, ok := codexPrincipalIdentity(token); ok {
			return "account " + account
		}
	}
	return ""
}

// Short non secret fingerprint suffix of the row's secret, empty when none is known
func loginFingerprint(e PooledCredential) string {
	fp := FingerprintSecretValue(e.AccessToken)
	if fp == "" {
		fp = e.ExtraString("secret_fingerprint")
	}
	if len(fp) < 6 {
		return ""
	}
	return fp[len(fp)-6:]
}

// Listed state of one row with the epoch it becomes usable again
func loginStatusOf(e PooledCredential, sole bool) (string, float64) {
	if e.AuthType == AuthTypeAPIKey && strings.TrimSpace(e.RuntimeAPIKey()) == "" {
		return LoginStatusUnavailable, 0
	}
	if e.LastStatus == StatusDead {
		if e.AuthType == AuthTypeOAuth {
			return LoginStatusNeedsSignIn, 0
		}
		return LoginStatusDead, 0
	}
	if until, ok := exhaustedUntil(e, sole); ok && until > nowEpoch() {
		if e.LastErrorCode == 401 && e.AuthType == AuthTypeOAuth {
			return LoginStatusNeedsSignIn, until
		}
		return LoginStatusLimited, until
	}
	return LoginStatusReady, 0
}

// LoginUsageWindow is one subscription usage window of a login
type LoginUsageWindow struct {
	Label       string
	UsedPercent float64
	ResetsAt    *time.Time
	Scope       string
}

// Login is one authenticated identity at one provider as listing surfaces show it, never carrying a secret
type Login struct {
	Provider    string
	ID          string
	Name        string
	Label       string
	AuthType    string
	Origin      string
	Source      string
	Account     string
	Fingerprint string
	Priority    int
	Rotate      bool
	Status      string
	ResetsAt    float64
	Reason      string
	// Profile default of the provider
	Default bool
	// Pinned by the session the listing context carries
	Session bool
	// The login the provider's next request would use
	Active bool
	// Other consumer of the same secret, git for GITHUB_TOKEN and GH_TOKEN
	SharedWith string
	Usage      []LoginUsageWindow
}

// LoginGroup is one provider's logins with its default, rotation strategy and the choice that applies
type LoginGroup struct {
	// Pool key the logins are stored under
	Provider string
	// Provider id the model picker lists, a custom endpoint's custom:<name>
	Slug        string
	Label       string
	Default     string
	Strategy    string
	OnExhausted string
	Choice      LoginChoice
	Logins      []Login
}

// Listing view of one row
func loginFromEntry(ctx context.Context, e PooledCredential, sole bool) Login {
	status, resets := loginStatusOf(e, sole)
	l := Login{Provider: e.Provider, ID: e.ID, Name: e.LoginName(), Label: e.Label, AuthType: e.AuthType, Origin: e.LoginOrigin(),
		Source: e.Source, Account: e.LoginAccount(), Fingerprint: loginFingerprint(e), Priority: e.Priority, Rotate: e.LoginRotate(),
		Status: status, ResetsAt: resets, Reason: e.LastErrorReason}
	if e.Source == "env:GITHUB_TOKEN" || e.Source == "env:GH_TOKEN" || e.Source == "gh_cli" {
		l.SharedWith = "git"
	}
	if ctx != nil && e.LastStatus != StatusDead && hasAccountUsage(ctx, e.Provider) {
		if snapshot := CachedAccountUsage(ctx, e.Provider, identityIDFor(e.Provider, e), ""); snapshot != nil && !snapshotIsStale(snapshot) {
			for _, w := range snapshot.Windows {
				if w.UsedPercent == nil {
					continue
				}
				l.Usage = append(l.Usage, LoginUsageWindow{Label: w.Label, UsedPercent: *w.UsedPercent, ResetsAt: w.ResetAt, Scope: w.Scope})
			}
		}
	}
	return l
}

// LoginEnv is the pool environment of the context's home and secret scope
func LoginEnv(ctx context.Context) PoolEnv { return modelsPoolEnv(ctx) }

// DefaultLoginID is the login a plain logout or disconnect removes, the profile default else the one in use, empty when none
func DefaultLoginID(env PoolEnv, provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if row, _, ok := logoutTargetLogin(env, provider); ok {
		return row.ID
	}
	return currentLoginID(profileLoginEnv(env), provider)
}

// SignedInProvider is the first registry provider, in picker order, that holds stored logins and reports signed in, among those keep accepts when set
func SignedInProvider(env PoolEnv, keep func(*Profile) bool) string {
	if env.Store == nil {
		return ""
	}
	pools, err := env.Store.ReadCredentialPools()
	if err != nil {
		env.log().Debug(fmt.Sprintf("Could not read the stored logins: %v", err))
		return ""
	}
	for _, row := range canonicalRegistryRows(env.ctx()) {
		if row == nil || len(pools[row.Name]) == 0 || (keep != nil && !keep(row)) {
			continue
		}
		if truthy(GetAuthStatus(env, row.Name)["logged_in"]) {
			return row.Name
		}
	}
	return ""
}

// ---- owning store ----

// Auth file holding the provider's rows, the profile's own when it has any, else the root it borrows
func owningPoolPath(env PoolEnv, provider string) string {
	if data, err := env.Store.Load(); err == nil && storeOwnsPoolProvider(data, provider) {
		return env.Store.Path()
	}
	if root := guardedGlobalRoot(env.Store.globalPath); root != "" {
		if data, err := env.Store.LoadFile(root); err == nil && storeOwnsPoolProvider(data, provider) {
			return root
		}
	}
	return env.Store.Path()
}

// Edits the provider's stored rows in place under the owning file's lock, saving when fn reports a change
func updatePoolRows(env PoolEnv, provider string, fn func(rows []map[string]any) (bool, error)) error {
	path := owningPoolPath(env, provider)
	unlock, err := env.Store.fileLock(path, AuthLockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := env.Store.LoadFile(path)
	if err != nil {
		return err
	}
	pool := section(data, "credential_pool")
	rows := poolEntries(pool[provider])
	changed, err := fn(rows)
	if err != nil || !changed {
		return err
	}
	list := make([]any, len(rows))
	for i, row := range rows {
		list[i] = row
	}
	pool[provider] = list
	return env.Store.SaveTo(data, path)
}

// Edits one stored row by id, refusing when the row is gone
func updateLoginRow(env PoolEnv, provider, id string, fn func(row map[string]any) bool) error {
	found := false
	err := updatePoolRows(env, provider, func(rows []map[string]any) (bool, error) {
		for _, row := range rows {
			if entryID(row) == id {
				found = true
				return fn(row), nil
			}
		}
		return false, nil
	})
	if err == nil && !found {
		return loginFault(LoginFaultUnknown, "%s login %s is no longer stored", provider, id)
	}
	return err
}

// Gives every row without a stored name a unique one and records token derived accounts, in memory and on disk
func ensureLoginNames(env PoolEnv, provider string, entries []PooledCredential) []PooledCredential {
	taken := map[string]bool{}
	for _, e := range entries {
		if name := strings.TrimSpace(e.ExtraString(loginNameKey)); name != "" {
			taken[name] = true
		}
	}
	names := map[string]string{}
	accounts := map[string]string{}
	for i, e := range entries {
		if strings.TrimSpace(e.ExtraString(loginNameKey)) == "" {
			name := uniqueLoginName(derivedLoginName(e), taken)
			taken[name] = true
			names[e.ID] = name
			entries[i] = e.WithExtra(loginNameKey, name)
		}
		if e.ExtraString(loginAccountKey) == "" && e.LoginOrigin() == LoginOriginOwned {
			if account := loginAccountFromToken(provider, e.AccessToken); account != "" {
				accounts[e.ID] = account
				entries[i] = entries[i].WithExtra(loginAccountKey, account)
			}
		}
	}
	if len(names) == 0 && len(accounts) == 0 {
		return entries
	}
	err := updatePoolRows(env, provider, func(rows []map[string]any) (bool, error) {
		changed := false
		for _, row := range rows {
			id := entryID(row)
			if name, ok := names[id]; ok && strings.TrimSpace(anyString(row[loginNameKey])) == "" {
				row[loginNameKey] = name
				changed = true
			}
			if account, ok := accounts[id]; ok && anyString(row[loginAccountKey]) == "" {
				row[loginAccountKey] = account
				changed = true
			}
		}
		return changed, nil
	})
	if err != nil {
		env.log().Debug(fmt.Sprintf("%s logins: could not store login names (%v)", provider, err))
	}
	return entries
}

// ---- listing ----

// ListLogins lists a provider's logins with the markers the selection, the profile default and rotation put on them
func ListLogins(env PoolEnv, provider string) LoginGroup {
	provider = strings.ToLower(strings.TrimSpace(provider))
	ctx := env.loginCtx()
	group := LoginGroup{Provider: provider, Slug: LoginPoolSlug(ctx, provider), Label: provider, Default: LoginDefault(ctx, provider),
		Strategy: GetPoolStrategy(ctx, provider), OnExhausted: LoginOnExhausted(ctx, provider)}
	if p, ok := registryProfileCtx(env.ctx(), provider); ok {
		group.Label = p.Label()
	}
	pool := LoadPool(env, provider)
	entries := ensureLoginNames(env, provider, pool.Entries())
	for _, e := range entries {
		pool.replaceEntry(e, false)
	}
	group.Choice = ResolveLoginChoice(env, provider, entries)
	activeID := ""
	view := SelectLoginView(env, pool)
	if cur, ok := view.Peek(); ok {
		activeID = cur.ID
	}
	defaultID := ""
	if row, ok := matchLoginRow(entries, group.Default); ok {
		defaultID = row.ID
	}
	live := 0
	for _, e := range entries {
		if e.LastStatus != StatusDead {
			live++
		}
	}
	for _, e := range entries {
		l := loginFromEntry(env.ctx(), e, live <= 1)
		l.Default = e.ID == defaultID
		l.Session = group.Choice.Scope == LoginScopeSession && group.Choice.LoginID == e.ID
		l.Active = e.ID == activeID
		group.Logins = append(group.Logins, l)
	}
	return group
}

// LoginPoolSlug is the provider id the model picker lists for a pool key, the custom:<name> slug of the endpoint of the providers setting ctx gives that owns the pool
func LoginPoolSlug(ctx context.Context, poolKey string) string {
	poolKey = poolKeyOf(poolKey)
	for _, ne := range iterCustomProviders(settings.Providers.Get(ctx)) {
		if slices.Contains(poolKeysForCustomEntry(ne.name, ne.entry), poolKey) {
			return CustomProviderSlug(cfgString(ne.entry["name"]), cfgString(ne.entry["provider_key"]))
		}
	}
	return poolKey
}

// IsCustomPoolKey reports a pool key that holds the logins of a custom endpoint of the providers setting ctx gives
func IsCustomPoolKey(ctx context.Context, poolKey string) bool {
	poolKey = poolKeyOf(poolKey)
	for _, ne := range iterCustomProviders(settings.Providers.Get(ctx)) {
		if slices.Contains(poolKeysForCustomEntry(ne.name, ne.entry), poolKey) {
			return true
		}
	}
	return poolKey == "custom" || strings.HasPrefix(poolKey, CustomPoolPrefix)
}

// LoginPoolKey maps a pool key or a provider slug a client sent to the pool key its logins are stored under for the settings ctx gives
func LoginPoolKey(ctx context.Context, provider string) string {
	key := poolKeyOf(provider)
	providers := settings.Providers.Get(ctx)
	if key == "custom" {
		return ResolveRuntimePoolKey(key, settings.ModelBaseURL.Get(ctx), providers)
	}
	for _, ne := range iterCustomProviders(providers) {
		keys := poolKeysForCustomEntry(ne.name, ne.entry)
		if len(keys) == 0 {
			continue
		}
		if slices.Contains(keys, key) || CustomProviderAliases(cfgString(ne.entry["name"]), cfgString(ne.entry["provider_key"]))[key] {
			return keys[0]
		}
	}
	return key
}

// Pool keys worth listing, every stored pool plus each provider the user configured
func loginProviderCandidates(env PoolEnv) []string {
	set := map[string]bool{}
	if pools, err := env.Store.ReadCredentialPools(); err == nil {
		for key, rows := range pools {
			if len(rows) > 0 {
				set[key] = true
			}
		}
	}
	for _, row := range canonicalRegistryRows(env.ctx()) {
		if row != nil && !set[row.Name] && IsProviderExplicitlyConfigured(env, row.Name) {
			set[row.Name] = true
		}
	}
	if !set["openrouter"] && GetEnvPreferDotenv(env, "OPENROUTER_API_KEY") != "" {
		set["openrouter"] = true
	}
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// ListAllLogins lists every provider that holds at least one login, or only the named one
func ListAllLogins(env PoolEnv, provider string) []LoginGroup {
	providers := loginProviderCandidates(env)
	if provider = strings.ToLower(strings.TrimSpace(provider)); provider != "" {
		providers = []string{provider}
	}
	var groups []LoginGroup
	for _, p := range providers {
		if group := ListLogins(env, p); len(group.Logins) > 0 {
			groups = append(groups, group)
		}
	}
	return groups
}

// FindLogin resolves a login of a provider by id, name, unique label or list index
func FindLogin(env PoolEnv, provider, target string) (PooledCredential, error) {
	entries := ensureLoginNames(env, provider, LoadPool(env, provider).Entries())
	if row, ok := matchLoginRow(entries, target); ok {
		return row, nil
	}
	if index, err := strconv.Atoi(strings.TrimSpace(target)); err == nil && index >= 1 && index <= len(entries) {
		return entries[index-1], nil
	}
	return PooledCredential{}, loginFault(LoginFaultUnknown, "no %s login matches %q. Run `icebath auth list %s` to see them", provider, target, provider)
}

// ---- config ----

// Sets or clears one logins.<provider>.<key> setting of the env's home through the settings writer
func setLoginsSetting(env PoolEnv, provider, key string, value any) error {
	ctx := home.WithOverride(env.ctx(), envHome(env))
	name := "logins." + poolKeyOf(provider) + "." + key
	var err error
	if value == nil || value == "" {
		_, err = settings.Reset(ctx, name, settings.Global)
	} else {
		_, err = settings.Set(ctx, name, value, settings.Global)
	}
	return err
}

// SetLoginDefault sets the provider's profile default to a login, @rotate, or none when empty, and returns the stored value
func SetLoginDefault(env PoolEnv, provider, target string) (string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	target = strings.TrimSpace(target)
	value := target
	switch target {
	case "":
	case LoginRotateSelection:
		if !slices.ContainsFunc(LoadPool(env, provider).Entries(), PooledCredential.LoginRotate) {
			return "", loginFault(LoginFaultValue, "no %s login is in rotation. Mark logins with `icebath auth rotation %s on` first", provider, provider)
		}
	default:
		row, err := FindLogin(env, provider, target)
		if err != nil {
			return "", err
		}
		value = row.ID
	}
	if err := setLoginsSetting(env, provider, "default", value); err != nil {
		return "", err
	}
	return value, nil
}

// SetLoginOnExhausted sets logins.<provider>.on_exhausted to fail or rotate
func SetLoginOnExhausted(env PoolEnv, provider, mode string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != LoginOnExhaustedFail && mode != LoginOnExhaustedRotate {
		return loginFault(LoginFaultValue, "on_exhausted takes fail or rotate, got %q", mode)
	}
	value := any(mode)
	if mode == LoginOnExhaustedFail {
		value = nil
	}
	return setLoginsSetting(env, strings.ToLower(strings.TrimSpace(provider)), "on_exhausted", value)
}

// SetLoginStrategy sets logins.<provider>.strategy, the order a rotation set is used in
func SetLoginStrategy(env PoolEnv, provider, strategy string) error {
	strategy = strings.ToLower(strings.TrimSpace(strategy))
	if !SupportedPoolStrategies[strategy] {
		return loginFault(LoginFaultValue, "unknown strategy %q. Use fill_first, round_robin, least_used or random", strategy)
	}
	return setLoginsSetting(env, strings.ToLower(strings.TrimSpace(provider)), "strategy", strategy)
}

// Pins the profile default once a provider gains a second login, so adding one never changes which account is used
func pinDefaultAfterAdd(env PoolEnv, provider, previousActiveID, newID string) {
	if previousActiveID == "" || previousActiveID == newID || LoginDefault(env.loginCtx(), provider) != "" {
		return
	}
	if err := setLoginsSetting(env, provider, "default", previousActiveID); err != nil {
		env.log().Debug(fmt.Sprintf("%s logins: could not pin the profile default (%v)", provider, err))
	}
}

// Login the provider's requests use under the profile default, empty when the pool serves none
func currentLoginID(env PoolEnv, provider string) string {
	if cur, ok := LoadSelectedPool(env, provider).Peek(); ok {
		return cur.ID
	}
	return ""
}

// ---- mutations ----

// AddLoginOptions shapes one new login
type AddLoginOptions struct {
	Name        string
	Rotate      bool
	MakeDefault bool
}

// Free name for a new login of the provider, the requested one validated
func newLoginName(provider, requested, fallback string, entries []PooledCredential) (string, error) {
	taken := map[string]bool{}
	for _, e := range entries {
		taken[e.LoginName()] = true
	}
	if requested = strings.TrimSpace(requested); requested != "" {
		if err := ValidateLoginName(requested); err != nil {
			return "", err
		}
		if taken[requested] {
			return "", loginFault(LoginFaultName, "%s already has a login named %q", provider, requested)
		}
		return requested, nil
	}
	return uniqueLoginName(SlugLoginName(fallback), taken), nil
}

// Refuses a requested login name that is invalid or taken at the provider
func checkNewLoginName(env PoolEnv, provider, name string) error {
	_, err := newLoginName(provider, name, "", LoadPool(env, provider).Entries())
	return err
}

// Endpoint a new login of the provider serves
func loginProviderBaseURL(env PoolEnv, provider string) string {
	return (&authCLI{ctx: env.ctx(), env: env}).providerBaseURL(provider)
}

// AddKeyLogin stores an API key as an owned login, updating the pasted key login of that name or key in place
func AddKeyLogin(env PoolEnv, provider, key string, o AddLoginOptions) (Login, bool, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	key = strings.TrimSpace(key)
	if provider == "" || key == "" {
		return Login{}, false, loginFault(LoginFaultValue, "provider and key are required")
	}
	pool := LoadPool(env, provider)
	entries := ensureLoginNames(env, provider, pool.Entries())
	previousActive := currentLoginID(env, provider)
	for _, e := range entries {
		named := o.Name != "" && e.LoginName() == o.Name
		sameKey := e.AccessToken == key
		if (!named && !sameKey) || e.Source != SourceManual {
			continue
		}
		err := updateLoginRow(env, provider, e.ID, func(row map[string]any) bool {
			row["access_token"] = key
			row["auth_type"] = normalizePoolAuthType(provider, key, AuthTypeAPIKey)
			for _, f := range poolStatusFields {
				row[f] = nil
			}
			row["status_cleared_at"] = nowEpoch()
			delete(row, "failure_reason")
			delete(row, "model_cooldowns")
			if o.Rotate {
				row[loginRotateKey] = true
			}
			return true
		})
		if err != nil {
			return Login{}, false, err
		}
		if o.MakeDefault {
			if _, err := SetLoginDefault(env, provider, e.ID); err != nil {
				return Login{}, false, err
			}
		}
		updated := e.ClearStatus()
		updated.AccessToken, updated.AuthType = key, normalizePoolAuthType(provider, key, AuthTypeAPIKey)
		return loginFromEntry(env.ctx(), updated, true), false, nil
	}
	name, err := newLoginName(provider, o.Name, provider+"-key", entries)
	if err != nil {
		return Login{}, false, err
	}
	added, err := pool.AddEntry(ownedKeyEntry(provider, key, name, loginProviderBaseURL(env, provider), o.Rotate))
	if err != nil {
		return Login{}, false, err
	}
	if o.MakeDefault {
		if _, err := SetLoginDefault(env, provider, added.ID); err != nil {
			return Login{}, true, err
		}
	} else {
		pinDefaultAfterAdd(env, provider, previousActive, added.ID)
	}
	return loginFromEntry(env.ctx(), added, true), true, nil
}

// Owned API key row of a new login
func ownedKeyEntry(provider, key, name, baseURL string, rotate bool) PooledCredential {
	entry := PooledCredential{Provider: provider, ID: newPoolID(), Label: name, AuthType: normalizePoolAuthType(provider, key, AuthTypeAPIKey), Source: SourceManual,
		AccessToken: key, BaseURL: baseURL, Extra: map[string]any{loginNameKey: name}}
	if rotate {
		entry.Extra[loginRotateKey] = true
	}
	return entry
}

// NameNewLogin names a login an auth add flow just stored, records its account and rotation flag, and pins the default
func NameNewLogin(env PoolEnv, provider, id, requested string, rotate bool, previousActiveID string) (Login, error) {
	var others []PooledCredential
	var self PooledCredential
	found := false
	for _, e := range LoadPool(env, provider).Entries() {
		if e.ID == id {
			self, found = e, true
			continue
		}
		others = append(others, e)
	}
	if !found {
		return Login{}, loginFault(LoginFaultUnknown, "%s login %s is no longer stored", provider, id)
	}
	account := loginAccountFromToken(provider, self.AccessToken)
	fallback := self.Label
	if account != "" {
		fallback = account
	}
	name, err := newLoginName(provider, requested, fallback, others)
	if err != nil {
		return Login{}, err
	}
	err = updateLoginRow(env, provider, id, func(row map[string]any) bool {
		row[loginNameKey] = name
		if account != "" {
			row[loginAccountKey] = account
		}
		if rotate {
			row[loginRotateKey] = true
		}
		return true
	})
	if err != nil {
		return Login{}, err
	}
	pinDefaultAfterAdd(env, provider, previousActiveID, id)
	self = self.WithExtra(loginNameKey, name)
	if account != "" {
		self = self.WithExtra(loginAccountKey, account)
	}
	if rotate {
		self = self.WithExtra(loginRotateKey, true)
	}
	return loginFromEntry(env.ctx(), self, true), nil
}

// RenameLogin gives a login a new unique name, references by id staying valid
func RenameLogin(env PoolEnv, provider, target, newName string) (Login, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	newName = strings.TrimSpace(newName)
	if err := ValidateLoginName(newName); err != nil {
		return Login{}, err
	}
	row, err := FindLogin(env, provider, target)
	if err != nil {
		return Login{}, err
	}
	for _, e := range LoadPool(env, provider).Entries() {
		if e.ID != row.ID && e.LoginName() == newName {
			return Login{}, loginFault(LoginFaultName, "%s already has a login named %q", provider, newName)
		}
	}
	err = updateLoginRow(env, provider, row.ID, func(stored map[string]any) bool {
		stored[loginNameKey] = newName
		return true
	})
	if err != nil {
		return Login{}, err
	}
	if def := LoginDefault(env.loginCtx(), provider); def != "" && def == row.LoginName() {
		if err := setLoginsSetting(env, provider, "default", row.ID); err != nil {
			return Login{}, err
		}
	}
	return loginFromEntry(env.ctx(), row.WithExtra(loginNameKey, newName), true), nil
}

// SetLoginRotation puts a login in or out of the provider's rotation set
func SetLoginRotation(env PoolEnv, provider, target string, on bool) (Login, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	row, err := FindLogin(env, provider, target)
	if err != nil {
		return Login{}, err
	}
	err = updateLoginRow(env, provider, row.ID, func(stored map[string]any) bool {
		if truthyAny(stored[loginRotateKey]) == on {
			return false
		}
		stored[loginRotateKey] = on
		return true
	})
	if err != nil {
		return Login{}, err
	}
	return loginFromEntry(env.ctx(), row.WithExtra(loginRotateKey, on), true), nil
}

// LoginRemoval reports one removed login, what its source's removal cleaned, and the default left behind
type LoginRemoval struct {
	Login      Login
	Cleaned    []string
	Hints      []string
	NewDefault string
}

// RemoveLogin removes one login and runs only its own source's removal step, repinning a default that named it
func RemoveLogin(env PoolEnv, provider, target string) (LoginRemoval, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	row, err := FindLogin(env, provider, target)
	if err != nil {
		return LoginRemoval{}, err
	}
	pool := LoadPool(env, provider)
	index := 0
	for i, e := range pool.Entries() {
		if e.ID == row.ID {
			index = i + 1
			break
		}
	}
	if index == 0 {
		return LoginRemoval{}, loginFault(LoginFaultUnknown, "%s login %s is no longer stored", provider, row.ID)
	}
	removed, ok := pool.RemoveIndex(index)
	if !ok {
		return LoginRemoval{}, fmt.Errorf("could not remove %s login %s", provider, row.LoginName())
	}
	out := LoginRemoval{Login: loginFromEntry(nil, removed, true)}
	out.Login.Name = row.LoginName()
	if step, found := FindRemovalStep(provider, removed.Source); found {
		result := step.Remove(env, provider, removed)
		out.Cleaned, out.Hints = result.Cleaned, result.Hints
		if result.Suppress {
			if err := env.Store.SuppressCredentialSource(provider, removed.Source); err != nil {
				return out, err
			}
		}
	}
	out.NewDefault, _ = repinDefaultAfterRemoval(env, provider, removed)
	return out, nil
}

// Points a profile default that named a removed login at the provider's first live login, reporting whether it moved
func repinDefaultAfterRemoval(env PoolEnv, provider string, removed PooledCredential) (string, bool) {
	def := LoginDefault(env.loginCtx(), provider)
	if def == "" || (def != removed.ID && def != removed.LoginName()) {
		return "", false
	}
	next := ""
	for _, e := range LoadPool(env, provider).Entries() {
		if e.LastStatus != StatusDead {
			next = e.ID
			break
		}
	}
	if err := setLoginsSetting(env, provider, "default", next); err != nil {
		env.log().Warn(fmt.Sprintf("%s logins: could not move the default off the removed login (%v)", provider, err))
		return "", false
	}
	return next, true
}

// LinkEnvLogin adds a login that reads its key from an env var Icebath does not own, reporting whether it is new
func LinkEnvLogin(env PoolEnv, provider, envVar, name string) (Login, bool, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	envVar = strings.TrimSpace(envVar)
	if envVar == "" || strings.ContainsAny(envVar, " =") {
		return Login{}, false, loginFault(LoginFaultValue, "invalid env var name %q", envVar)
	}
	token := GetEnvPreferDotenv(env, envVar)
	if token == "" {
		return Login{}, false, loginFault(LoginFaultValue, "%s is not set in the profile .env, a secret source, or the environment", envVar)
	}
	source := "env:" + envVar
	if _, err := env.Store.UnsuppressCredentialSource(provider, source); err != nil {
		return Login{}, false, err
	}
	pool := LoadPool(env, provider)
	previousActive := currentLoginID(env, provider)
	entries := pool.Entries()
	for _, e := range entries {
		if e.Source == source {
			if name != "" && name != e.LoginName() {
				renamed, err := RenameLogin(env, provider, e.ID, name)
				return renamed, false, err
			}
			return loginFromEntry(env.ctx(), e, true), false, nil
		}
	}
	loginName, err := newLoginName(provider, name, envVar, entries)
	if err != nil {
		return Login{}, false, err
	}
	payload := envPayload(env, envVar, token, loginProviderBaseURL(env, provider))
	payload["source"] = source
	payload[loginNameKey] = loginName
	added, err := pool.AddEntry(PooledCredentialFromMap(provider, payload))
	if err != nil {
		return Login{}, false, err
	}
	pinDefaultAfterAdd(env, provider, previousActive, added.ID)
	return loginFromEntry(env.ctx(), added, true), true, nil
}

// LinkExternalLogin lets a borrowed login of another tool, claude_code or qwen-cli, seed again, reporting whether it was suppressed
func LinkExternalLogin(env PoolEnv, provider, source string) (Login, bool, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	unsuppressed, err := env.Store.UnsuppressCredentialSource(provider, source)
	if err != nil {
		return Login{}, false, err
	}
	previousActive := currentLoginID(env, provider)
	for _, e := range LoadPool(env, provider).Entries() {
		if e.Source == source {
			pinDefaultAfterAdd(env, provider, previousActive, e.ID)
			return loginFromEntry(env.ctx(), e, true), unsuppressed, nil
		}
	}
	if source == "claude_code" && !AdoptExternalLoginsEnabled(env) {
		return Login{}, false, loginFault(LoginFaultValue, "auth.adopt_external_logins is false, so the Claude Code login is not adopted")
	}
	return Login{}, false, loginFault(LoginFaultValue, "no %s login was found to link for %s", source, provider)
}

// ConvertLinkedLogin moves an env linked API key into an owned login of the same name and removes the env source
func ConvertLinkedLogin(env PoolEnv, provider, target string) (Login, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	row, err := FindLogin(env, provider, target)
	if err != nil {
		return Login{}, err
	}
	if !strings.HasPrefix(row.Source, "env:") || row.AuthType != AuthTypeAPIKey {
		return Login{}, loginFault(LoginFaultValue, "%s login %s is not an env linked API key", provider, row.LoginName())
	}
	token := strings.TrimSpace(row.AccessToken)
	if token == "" {
		token = GetEnvPreferDotenv(env, strings.TrimPrefix(row.Source, "env:"))
	}
	if token == "" {
		return Login{}, loginFault(LoginFaultValue, "%s is empty, nothing to convert", strings.TrimPrefix(row.Source, "env:"))
	}
	name, def := row.LoginName(), LoginDefault(env.loginCtx(), provider)
	added, err := LoadPool(env, provider).AddEntry(ownedKeyEntry(provider, token, name, row.BaseURL, row.LoginRotate()))
	if err != nil {
		return Login{}, err
	}
	if _, err := RemoveLogin(env, provider, row.ID); err != nil {
		return Login{}, err
	}
	if def == row.ID || def == name {
		if _, err := SetLoginDefault(env, provider, added.ID); err != nil {
			return Login{}, err
		}
	}
	return loginFromEntry(env.ctx(), added, true), nil
}

// ---- icebath model ----

// Stores a key icebath model was given as the provider's default owned login, LM Studio's no-auth placeholder staying in .env
func saveFlowKeyLogin(env PoolEnv, provider, keyEnv, key string) error {
	if provider == "" || provider == "lmstudio" || key == LMStudioNoAuthPlaceholder {
		return config.SaveEnvValue(envHome(env), keyEnv, key)
	}
	current := ""
	if id := DefaultLoginID(env, provider); id != "" {
		if row, err := FindLogin(env, provider, id); err == nil && row.Source == SourceManual && row.AuthType == AuthTypeAPIKey {
			current = row.LoginName()
		}
	}
	_, _, err := AddKeyLogin(env, provider, key, AddLoginOptions{Name: current, MakeDefault: true})
	return err
}

// Stores an Anthropic API key or setup-token as the default owned login
func saveAnthropicFlowLogin(env PoolEnv, token string) error {
	_, _, err := AddKeyLogin(env, "anthropic", token, AddLoginOptions{MakeDefault: true})
	return err
}

// Links the Claude Code login and makes it the Anthropic default, false when it could not be linked
func linkClaudeCodeFlowLogin(env PoolEnv) bool {
	login, _, err := LinkExternalLogin(env, "anthropic", "claude_code")
	if err != nil {
		return false
	}
	_, err = SetLoginDefault(env, "anthropic", login.ID)
	return err == nil
}
