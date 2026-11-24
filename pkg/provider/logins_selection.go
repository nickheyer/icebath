package provider

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
)

// LoginRotateSelection names the rotation set of a provider
const LoginRotateSelection = "@rotate"

// LoginAnyProvider keys a selection for whichever provider holds a login of that id or name, or a rotation set for @rotate
const LoginAnyProvider = "*"

// Scopes a login selection resolves from, most specific first
const (
	LoginScopeSession = "session"
	LoginScopeProfile = "profile"
	LoginScopeLegacy  = "legacy"
)

// Headless reactions to an exhausted pinned login
const (
	LoginOnExhaustedFail   = "fail"
	LoginOnExhaustedRotate = "rotate"
)

type loginSelectionKey struct{}

// model_config keys holding a session's own login choices
const (
	ModelConfigLogins           = "logins"
	ModelConfigLoginOnExhausted = "login_on_exhausted"
	ModelConfigLoginWait        = "login_wait"
)

// SessionLogins is one session's own login choices per pool key, the pins and the on_exhausted overrides
type SessionLogins struct {
	// Login id, or @rotate, pinned per pool key
	Pins map[string]string `json:"pins,omitempty"`
	// rotate or fail per pool key, overriding logins.<p>.on_exhausted for this session only
	OnExhausted map[string]string `json:"on_exhausted,omitempty"`
}

// Normalized pool key of a provider name
func poolKeyOf(provider string) string { return strings.ToLower(strings.TrimSpace(provider)) }

// Clone copies both maps, nil maps staying empty
func (s SessionLogins) Clone() SessionLogins {
	out := SessionLogins{Pins: map[string]string{}, OnExhausted: map[string]string{}}
	for k, v := range s.Pins {
		out.Pins[k] = v
	}
	for k, v := range s.OnExhausted {
		out.OnExhausted[k] = v
	}
	return out
}

// Empty reports a session that follows the profile for every provider
func (s SessionLogins) Empty() bool { return len(s.Pins) == 0 && len(s.OnExhausted) == 0 }

// Key is a stable text of the choices, equal for equal choices
func (s SessionLogins) Key() string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(s.Pins)) {
		b.WriteString("p:" + k + "=" + s.Pins[k] + ";")
	}
	for _, k := range slices.Sorted(maps.Keys(s.OnExhausted)) {
		b.WriteString("x:" + k + "=" + s.OnExhausted[k] + ";")
	}
	return b.String()
}

// With is a copy with one pool key's pin and exhaustion override changed, nil leaving that part as it is and "" clearing it
func (s SessionLogins) With(pool string, pin, onExhausted *string) SessionLogins {
	out := s.Clone()
	pool = poolKeyOf(pool)
	if pool == "" {
		return out
	}
	if pin != nil {
		if v := strings.TrimSpace(*pin); v != "" {
			out.Pins[pool] = v
		} else {
			delete(out.Pins, pool)
		}
	}
	if onExhausted != nil {
		switch v := strings.ToLower(strings.TrimSpace(*onExhausted)); v {
		case LoginOnExhaustedRotate, LoginOnExhaustedFail:
			out.OnExhausted[pool] = v
		default:
			delete(out.OnExhausted, pool)
		}
	}
	return out
}

// Text map of a model_config value, normalized keys and trimmed values, nil for anything else
func textMap(raw any) map[string]string {
	m, _ := raw.(map[string]any)
	out := map[string]string{}
	for k, v := range m {
		key, text := poolKeyOf(k), strings.TrimSpace(anyString(v))
		if key != "" && text != "" {
			out[key] = text
		}
	}
	return out
}

// SessionLoginsFromModelConfig reads a session's login choices from its stored model_config
func SessionLoginsFromModelConfig(cfg map[string]any) SessionLogins {
	s := SessionLogins{Pins: textMap(cfg[ModelConfigLogins]), OnExhausted: map[string]string{}}
	for k, v := range textMap(cfg[ModelConfigLoginOnExhausted]) {
		if v = strings.ToLower(v); v == LoginOnExhaustedRotate || v == LoginOnExhaustedFail {
			s.OnExhausted[k] = v
		}
	}
	return s
}

// StoreInModelConfig writes the choices into a model_config map, an empty part kept as an empty map so a later whole config rewrite cannot bring old choices back
func (s SessionLogins) StoreInModelConfig(cfg map[string]any) {
	store := func(key string, m map[string]string) {
		out := make(map[string]any, len(m))
		for k, v := range m {
			out[k] = v
		}
		cfg[key] = out
	}
	store(ModelConfigLogins, s.Pins)
	store(ModelConfigLoginOnExhausted, s.OnExhausted)
}

// WithSessionLogins puts a session's own login choices on the context, replacing any it carried
func WithSessionLogins(ctx context.Context, s SessionLogins) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, loginSelectionKey{}, s.Clone())
}

// SessionLoginsFrom copies the session login choices the context carries, false when it carries none
func SessionLoginsFrom(ctx context.Context) (SessionLogins, bool) {
	if ctx == nil {
		return SessionLogins{}.Clone(), false
	}
	held, ok := ctx.Value(loginSelectionKey{}).(SessionLogins)
	return held.Clone(), ok
}

// WithLoginSelection adds pins by pool key to the context's session choices, an empty login dropping that key
func WithLoginSelection(ctx context.Context, selection map[string]string) context.Context {
	current, _ := SessionLoginsFrom(ctx)
	for k, v := range selection {
		current = current.With(k, &v, nil)
	}
	return WithSessionLogins(ctx, current)
}

// Context with no session login choices, so only the profile default applies
func withoutLoginSelection(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	return WithSessionLogins(ctx, SessionLogins{})
}

// Pool environment that ignores the session's login choices
func profileLoginEnv(env PoolEnv) PoolEnv {
	env.Ctx = withoutLoginSelection(env.Ctx)
	return env
}

// LoginSelectionFrom copies the pins the context carries, empty when none
func LoginSelectionFrom(ctx context.Context) map[string]string {
	held, _ := SessionLoginsFrom(ctx)
	return held.Pins
}

// LoginChoice is the login or rotation set one provider's requests use, with the scope it came from
type LoginChoice struct {
	Provider string
	// Pinned login id, empty for the rotation set and legacy resolution
	LoginID string
	Rotate  bool
	Scope   string
	// Session selection that names no login of the provider any more
	Missing           string
	OnExhaustedRotate bool
}

// Legacy reports a choice that leaves the whole pool to the strategy and failover
func (c LoginChoice) Legacy() bool { return c.LoginID == "" && !c.Rotate }

// Pinned reports a choice of exactly one login
func (c LoginChoice) Pinned() bool { return c.LoginID != "" }

// logins.<provider> settings of the home in ctx, the provider matched without case, nil when absent
func loginsProviderConfig(ctx context.Context, provider string) map[string]any {
	logins := settings.Logins.Get(ctx)
	provider = strings.ToLower(strings.TrimSpace(provider))
	if m, ok := logins[provider].(map[string]any); ok {
		return m
	}
	for k, v := range logins {
		if strings.ToLower(strings.TrimSpace(k)) == provider {
			m, _ := v.(map[string]any)
			return m
		}
	}
	return nil
}

// LoginDefault is logins.<provider>.default, a login id, a login name or @rotate, empty when unset
func LoginDefault(ctx context.Context, provider string) string {
	return strings.TrimSpace(anyString(loginsProviderConfig(ctx, provider)["default"]))
}

// LoginOnExhausted is logins.<provider>.on_exhausted, fail unless rotate is configured
func LoginOnExhausted(ctx context.Context, provider string) string {
	if anyString(loginsProviderConfig(ctx, provider)["on_exhausted"]) == LoginOnExhaustedRotate {
		return LoginOnExhaustedRotate
	}
	return LoginOnExhaustedFail
}

// Strategy logins.<provider>.strategy names, empty when unset
func loginStrategy(ctx context.Context, provider string) string {
	return anyString(loginsProviderConfig(ctx, provider)["strategy"])
}

// Row a selection value names by id, then by name, then by a unique label, false when none does
func matchLoginRow(entries []PooledCredential, value string) (PooledCredential, bool) {
	value = strings.TrimSpace(value)
	if value == "" || value == LoginRotateSelection {
		return PooledCredential{}, false
	}
	for _, e := range entries {
		if e.ID == value {
			return e, true
		}
	}
	lower := strings.ToLower(value)
	for _, e := range entries {
		if strings.ToLower(e.LoginName()) == lower {
			return e, true
		}
	}
	var labelled []PooledCredential
	for _, e := range entries {
		if strings.ToLower(strings.TrimSpace(e.Label)) == lower {
			labelled = append(labelled, e)
		}
	}
	if len(labelled) == 1 {
		return labelled[0], true
	}
	return PooledCredential{}, false
}

var (
	loginMissingWarnedMu sync.Mutex
	loginMissingWarned   = map[string]bool{}
)

// Warns once per process for each missing pinned login
func warnMissingLoginOnce(env PoolEnv, key, message string) {
	loginMissingWarnedMu.Lock()
	defer loginMissingWarnedMu.Unlock()
	if loginMissingWarned[key] {
		return
	}
	loginMissingWarned[key] = true
	env.log().Warn(message)
}

// Context the login choice reads settings through, the store's home bound onto the env's context
func (env PoolEnv) loginCtx() context.Context {
	return env.hookContext()
}

// SessionOnExhausted is the reaction of a session's exhausted pinned login, its own override first, then the profile's
func SessionOnExhausted(env PoolEnv, provider string) (string, string) {
	provider = poolKeyOf(provider)
	held, _ := SessionLoginsFrom(env.ctx())
	if v := held.OnExhausted[provider]; v != "" {
		return v, LoginScopeSession
	}
	return LoginOnExhausted(env.loginCtx(), provider), LoginScopeProfile
}

// ResolveLoginChoice picks the provider's login, the session's selection first, then the profile default, else legacy
func ResolveLoginChoice(env PoolEnv, provider string, entries []PooledCredential) LoginChoice {
	provider = poolKeyOf(provider)
	reaction, _ := SessionOnExhausted(env, provider)
	choice := LoginChoice{Provider: provider, Scope: LoginScopeLegacy, OnExhaustedRotate: reaction == LoginOnExhaustedRotate}
	if len(entries) == 0 {
		return choice
	}
	selection := LoginSelectionFrom(env.ctx())
	value, keyed := selection[provider]
	if !keyed {
		value = selection[LoginAnyProvider]
	}
	switch {
	case value == "":
	case value == LoginRotateSelection:
		if keyed || slices.ContainsFunc(entries, PooledCredential.LoginRotate) {
			choice.Rotate, choice.Scope = true, LoginScopeSession
			return choice
		}
	default:
		if row, found := matchLoginRow(entries, value); found {
			choice.LoginID, choice.Scope = row.ID, LoginScopeSession
			return choice
		}
		if keyed {
			choice.Missing = value
			warnMissingLoginOnce(env, "session:"+provider+":"+value, fmt.Sprintf("%s: the session's login %q no longer exists; using the profile default instead", provider, value))
		}
	}
	def := LoginDefault(env.loginCtx(), provider)
	switch {
	case def == "":
	case def == LoginRotateSelection:
		choice.Rotate, choice.Scope = true, LoginScopeProfile
		return choice
	default:
		if row, found := matchLoginRow(entries, def); found {
			choice.LoginID, choice.Scope = row.ID, LoginScopeProfile
			return choice
		}
		warnMissingLoginOnce(env, "profile:"+provider+":"+def, fmt.Sprintf("%s: logins.%s.default names %q, which no longer exists; every login of the provider is used until a new default is set", provider, provider, def))
	}
	return choice
}

// Rows a choice serves and the rows an on_exhausted rotate may fall over to
func loginChoiceRows(choice LoginChoice, entries []PooledCredential) ([]PooledCredential, []PooledCredential) {
	var rows, rotation, others []PooledCredential
	for _, e := range entries {
		switch {
		case choice.Pinned() && e.ID == choice.LoginID:
			rows = append(rows, e)
		case choice.Rotate && e.LoginRotate():
			rows = append(rows, e)
		case e.LoginRotate():
			rotation = append(rotation, e)
		default:
			others = append(others, e)
		}
	}
	if !choice.Pinned() {
		return rows, nil
	}
	if len(rotation) > 0 {
		return rows, rotation
	}
	return rows, others
}

// SelectLoginView narrows a loaded pool to the login its selection names, the pool itself when nothing is selected
func SelectLoginView(env PoolEnv, pool *Pool) *Pool {
	if pool == nil {
		return nil
	}
	entries := pool.Entries()
	choice := ResolveLoginChoice(env, pool.provider, entries)
	if choice.Legacy() {
		if choice.Missing != "" {
			pool.mu.Lock()
			pool.choice = &choice
			pool.mu.Unlock()
		}
		return pool
	}
	rows, spare := loginChoiceRows(choice, entries)
	view := NewPool(pool.env, pool.provider, rows)
	pool.mu.Lock()
	for id, pair := range pool.persistedTokenPairs {
		view.persistedTokenPairs[id] = pair
	}
	for id := range pool.borrowedRootIDs {
		view.borrowedRootIDs[id] = true
	}
	pool.mu.Unlock()
	view.choice = &choice
	if choice.OnExhaustedRotate {
		view.spare = spare
	}
	return view
}

// LoadSelectedPool loads a provider's pool narrowed to the login the context's session or the profile default selects
func LoadSelectedPool(env PoolEnv, provider string) *Pool {
	return SelectLoginView(env, LoadPool(env, provider))
}

// LoginChoice is the selection the pool was narrowed to, false for a whole legacy pool
func (p *Pool) LoginChoice() (LoginChoice, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.choice == nil {
		return LoginChoice{}, false
	}
	return *p.choice, true
}

// Pinned reports a pool narrowed to one login that never fails over to another account
func (p *Pool) Pinned() bool {
	choice, ok := p.LoginChoice()
	return ok && choice.Pinned() && !choice.Rotate
}

// Folds the on_exhausted spare rows into an exhausted pinned view, caller holds the lock
func (p *Pool) widenOnExhausted() bool {
	if len(p.spare) == 0 || p.choice == nil {
		return false
	}
	name := p.choice.LoginID
	if row, ok := p.findByID(p.choice.LoginID); ok {
		name = row.LoginName()
	}
	for _, e := range p.spare {
		e.scope = p.env.Scope
		p.entries = append(p.entries, e)
	}
	sort.SliceStable(p.entries, func(i, j int) bool { return p.entries[i].Priority < p.entries[j].Priority })
	p.spare = nil
	p.choice.Rotate = true
	p.env.log().Warn(fmt.Sprintf("credential pool: %s login %s is exhausted and logins.%s.on_exhausted is rotate; rotating to the provider's other logins", p.provider, name, p.provider))
	return true
}

// Codes of the refusal a selected login or rotation set with nothing usable answers
const (
	LoginExhaustedCode     = "login_exhausted"
	LoginRotationEmptyCode = "login_rotation_empty"
)

// Typed refusal for a provider whose selected login or rotation set has nothing usable, nil when the choice is legacy
func loginUnavailableError(pool *Pool, model string) *AuthError {
	if pool == nil {
		return nil
	}
	choice, ok := pool.LoginChoice()
	if !ok || choice.Legacy() {
		return nil
	}
	provider := pool.Provider()
	when := ""
	if at, known := pool.NextAvailableAt(model); known {
		when = " until " + at.Local().Format("15:04")
	}
	if choice.Rotate {
		if pool.EntryCount() == 0 {
			return newAuthErr(provider, fmt.Sprintf("No %s logins are in rotation. Mark logins with `icebath auth rotation %s on` "+
				"or pick one with `icebath auth use %s <login>`.", provider, provider, provider), LoginRotationEmptyCode, true)
		}
		return newAuthErr(provider, fmt.Sprintf("Every %s login in rotation is unavailable%s.", provider, when), LoginExhaustedCode, false)
	}
	name := choice.LoginID
	if row, found := pool.findEntry(choice.LoginID); found {
		name = row.LoginName()
	}
	return newAuthErr(provider, fmt.Sprintf("The %s login %q is unavailable%s and is pinned, so Icebath will not switch accounts. "+
		"Pick another with `/model --login <name>` or `icebath auth use %s <name>`, or set logins.%s.on_exhausted: rotate.",
		provider, name, when, provider, provider), LoginExhaustedCode, false)
}

// LoginUnavailable is the typed refusal of a pool whose selected login or rotation set has nothing usable for model, nil when the choice is legacy
func (p *Pool) LoginUnavailable(model string) error {
	if err := loginUnavailableError(p, model); err != nil {
		return err
	}
	return nil
}

// Keep makes the row of an id the pool's current one while it is still usable for model, false otherwise
func (p *Pool) Keep(id, model string) (PooledCredential, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	available, _ := p.availableEntries(false, false, model)
	for _, e := range available {
		if e.ID == id {
			p.currentID = id
			return e, true
		}
	}
	return PooledCredential{}, false
}

// LoginStateStamp changes whenever the home's logins, defaults or rotation may have changed, in this process or another
func LoginStateStamp(env PoolEnv) string {
	if env.Store == nil {
		return ""
	}
	return fileSignature(env.Store.Path()) + "|" + fileSignature(home.ConfigPath(env.Store.Home())) + "|" + fileSignature(env.Store.GlobalPath()) +
		"|" + fileSignature(home.EnvPath(env.Store.Home()))
}

// Row by id under the pool lock
func (p *Pool) findEntry(id string) (PooledCredential, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.findByID(id)
}

// LoginRef names one login by id and name
type LoginRef struct {
	ID   string
	Name string
}

// LoginExhaustedEvent reports a pinned login that ran out, its reset time and the provider's other ready logins
type LoginExhaustedEvent struct {
	SessionID string
	// Pool key of the login
	Provider string
	// Provider id the model picker lists
	Slug     string
	Login    LoginRef
	ResetsAt float64
	Reason   string
	Ready    []LoginRef
	// The provider has other logins in its rotation set
	Rotation bool
	// What the session does when the login runs out, fail unless a rotate fell over already
	OnExhausted string
}

// LoginExhaustion describes the pinned login of a pool that failed, nil when the pool is not pinned
func LoginExhaustion(pool *Pool, credentialID string) *LoginExhaustedEvent {
	if pool == nil || !pool.Pinned() {
		return nil
	}
	choice, _ := pool.LoginChoice()
	id := credentialID
	if id == "" {
		id = choice.LoginID
	}
	row, found := pool.findEntry(id)
	if !found {
		return nil
	}
	reaction, _ := SessionOnExhausted(pool.env, pool.provider)
	ev := &LoginExhaustedEvent{Provider: pool.Provider(), Slug: LoginPoolSlug(pool.env.loginCtx(), pool.provider),
		Login: LoginRef{ID: row.ID, Name: row.LoginName()}, Reason: row.LastErrorReason, OnExhausted: reaction}
	if until, ok := exhaustedUntil(row, true); ok {
		ev.ResetsAt = until
	}
	full := LoadPool(pool.env, pool.provider)
	full.mu.Lock()
	available, _ := full.availableEntries(false, false, "")
	for _, e := range full.entries {
		if e.ID != row.ID && e.LoginRotate() {
			ev.Rotation = true
		}
	}
	full.mu.Unlock()
	for _, e := range available {
		if e.ID != row.ID {
			ev.Ready = append(ev.Ready, LoginRef{ID: e.ID, Name: e.LoginName()})
		}
	}
	return ev
}

// PinnedLoginExhaustion describes the login a refusal of an exhausted pinned selection names, nil when the selection pins none
func PinnedLoginExhaustion(env PoolEnv, err error) *LoginExhaustedEvent {
	var refused *AuthError
	if !errors.As(err, &refused) || refused.Code != LoginExhaustedCode {
		return nil
	}
	return LoginExhaustion(LoadSelectedPool(env, refused.Provider), "")
}

// LoginResetAt is when a provider's login is usable again, false when it is usable now or the time is unknown
func LoginResetAt(env PoolEnv, provider, id string) (float64, bool) {
	pool := LoadPool(env, provider)
	row, found := pool.findEntry(id)
	if !found || row.LastStatus != StatusExhausted {
		return 0, false
	}
	until, ok := exhaustedUntil(row, true)
	if !ok || until <= nowEpoch() {
		return 0, false
	}
	return until, true
}

// Stored rows narrowed to the login or rotation set the selection names, every row when the choice is legacy
func selectedLoginRows(env PoolEnv, provider string, rows []map[string]any) []map[string]any {
	entries := make([]PooledCredential, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, PooledCredentialFromMap(provider, row))
	}
	choice := ResolveLoginChoice(env, provider, entries)
	if choice.Legacy() {
		return rows
	}
	selected, _ := loginChoiceRows(choice, entries)
	keep := map[string]bool{}
	for _, e := range selected {
		keep[e.ID] = true
	}
	out := make([]map[string]any, 0, len(selected))
	for _, row := range rows {
		if keep[entryID(row)] {
			out = append(out, row)
		}
	}
	return out
}
