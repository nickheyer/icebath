package credentials

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// States an account's credential can be in, as icebath last found it
const (
	// Waiting out a limit
	Exhausted = "exhausted"
	// Refused for good, until signed in again
	Dead = "dead"
)

// How long an account waits out a limit the provider gave no end for
const (
	ttlAuth    = 5 * time.Minute
	ttlLimit   = time.Hour
	ttlDefault = time.Hour
	// An account with nothing else to rotate to is tried again soon
	ttlSole = time.Minute
	// A model the account's plan doesn't include stays benched until its limits are reset by hand
	ttlEntitlement = 365 * 24 * time.Hour
	// Renewals of one account a run of refusals may try before its failure is left to the caller
	maxRenewals = 2
	// Longest a renewal may wait for another process renewing the same account
	renewWait = 25 * time.Second
)

// Reasons an OAuth refusal names that mean the credential is gone for good
var terminal = map[string]bool{"token_invalidated": true, "token_revoked": true, "invalid_token": true, "invalid_grant": true,
	"unauthorized_client": true, "refresh_token_reused": true}

// Codex refuses a model its ChatGPT plan doesn't include with this
const entitlementMarker = "model is not supported when using codex with a chatgpt account"

// Ways a refusal says when its limit lifts
var (
	quotaDelay    = regexp.MustCompile(`(?i)quotaResetDelay[:\s"]+(\d+(?:\.\d+)?)(ms|s)`)
	resetsIn      = regexp.MustCompile(`(?i)resets?\s+in\s+(?:(\d+(?:\.\d+)?)\s*(?:h|hr|hrs|hour|hours)\b\s*)?(?:(\d+(?:\.\d+)?)\s*(?:m|min|mins|minute|minutes)\b\s*)?(?:(\d+(?:\.\d+)?)\s*(?:s|sec|secs|second|seconds)\b)?`)
	retryAfter    = regexp.MustCompile(`(?i)retry\s+(?:after\s+)?(\d+(?:\.\d+)?)\s*(?:sec|secs|seconds|s\b)`)
	resetsSeconds = regexp.MustCompile(`(?i)resets_in_seconds\W{1,4}(\d+(?:\.\d+)?)`)
)

// Registrant whose credentials expire and that it renews, such as an OAuth sign in
type Renewer interface {
	// Whether the account's credential is close enough to expiring to be renewed before it is used
	Due(account Account) bool
	// Renews the account's credential now, keeping what it renewed in the account's fields or where the program it came from keeps it;
	// false when the account holds nothing it renews
	Renew(ctx context.Context, account Account) (bool, error)
}

// Registrant that knows refusals besides a 401 which renewing its credentials fixes, such as a Copilot token a model was moved off
type Staler interface {
	Stale(account Account, failure *provider.APIError) bool
}

// Registrant that can tell an account's limit lifted before the time it was given, such as a Codex plan whose window reset early
type Restorer interface {
	Restored(ctx context.Context, account Account) bool
}

// Registrant that can say why it has no credential for an account of a kind it keeps, such as what the token service it asked answered;
// false when it has nothing to say
type Explainer interface {
	Why(account Account) (string, bool)
}

// Why the account has no credential, as its registrant says; empty when none says
func (r *Registry) why(account Account) string {
	for _, registrant := range r.registrants {
		if explainer, ok := registrant.(Explainer); ok {
			if reason, ok := explainer.Why(account); ok {
				return reason
			}
		}
	}
	return ""
}

// What the registries of one host share: the config they renew credentials under, the renewals and leases under way
type pool struct {
	config   *config.Config
	mu       sync.Mutex
	renewing map[string]*sync.Mutex
	leases   map[string]int
}

func newPool() *pool {
	return &pool{renewing: map[string]*sync.Mutex{}, leases: map[string]int{}}
}

// Key of an account across the profile
func key(account Account) string {
	if account.Name == "" {
		return account.Provider.Settings.Prefix() + "#" + account.Spec()
	}
	return account.Fields.Prefix()
}

// Holds the account for one renewal, in this process and across processes; the releaser
func (p *pool) hold(ctx context.Context, account Account) (func(), error) {
	if p == nil {
		return func() {}, nil
	}
	k := key(account)
	p.mu.Lock()
	local, ok := p.renewing[k]
	if !ok {
		local = &sync.Mutex{}
		p.renewing[k] = local
	}
	p.mu.Unlock()
	local.Lock()
	if p.config == nil {
		return local.Unlock, nil
	}
	sum := sha256.Sum256([]byte(k))
	unlock, err := fsx.Lock(ctx, filepath.Join(p.config.Dir(), "locks", "credentials", hex.EncodeToString(sum[:8])+".lock"), renewWait)
	if err != nil {
		local.Unlock()
		return nil, err
	}
	return func() { unlock(); local.Unlock() }, nil
}

// What icebath last found of an account's limits
type Status struct {
	// Exhausted while it waits out a limit, Dead once its credential was refused for good, empty while nothing stands against it
	State   string
	At      time.Time
	Code    int
	Reason  string
	Message string
	// When the limit lifts, zero when the provider didn't say
	Resets time.Time
	// Models the account may not use until the time each gives, its other models unaffected
	Cooldowns map[string]time.Time
}

func millis(value any) time.Time {
	if ms := cast.ToInt64(value); ms > 0 {
		return time.UnixMilli(ms)
	}
	return time.Time{}
}

func stamp(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// Status kept in the account's fields
func StatusOf(account Account) Status {
	if account.Name == "" {
		return Status{}
	}
	stored := cast.ToStringMap(account.Fields.Get(providerconfig.Status))
	status := Status{State: cast.ToString(stored["state"]), At: millis(stored["at"]), Code: cast.ToInt(stored["code"]), Reason: cast.ToString(stored["reason"]),
		Message: cast.ToString(stored["message"]), Resets: millis(stored["resets"]), Cooldowns: map[string]time.Time{}}
	for _, row := range cast.ToSlice(stored["cooldowns"]) {
		entry := cast.ToStringMap(row)
		if model, until := cast.ToString(entry["model"]), millis(entry["until"]); model != "" && !until.IsZero() {
			status.Cooldowns[model] = until
		}
	}
	return status
}

// Keeps the status in the account's fields, none when nothing stands against it
func (s Status) save(account Account) error {
	if account.Name == "" {
		return nil
	}
	var cooldowns []any
	for model, until := range s.Cooldowns {
		if time.Now().Before(until) {
			cooldowns = append(cooldowns, map[string]any{"model": model, "until": until.UnixMilli()})
		}
	}
	if s.State == "" && len(cooldowns) == 0 {
		return account.Fields.Set(providerconfig.Status, nil)
	}
	stored := map[string]any{"state": s.State, "at": stamp(s.At), "code": s.Code, "reason": s.Reason, "message": s.Message, "resets": stamp(s.Resets)}
	if len(cooldowns) > 0 {
		stored["cooldowns"] = cooldowns
	}
	return account.Fields.Set(providerconfig.Status, stored)
}

// Wait of a limit with no end given, by what refused the account and whether it has another to rotate to
func ttl(code int, reason string, sole bool) time.Duration {
	if code == 401 {
		return ttlAuth
	}
	base := ttlDefault
	if code == 429 {
		base = ttlLimit
	}
	if reason == "billing_unverified" && code != 402 {
		return min(base, ttlSole)
	}
	if sole && code != 402 && reason != "billing" {
		return min(base, ttlSole)
	}
	return base
}

// When an exhausted account is tried again, zero for one that isn't exhausted
func (s Status) Until(sole bool) time.Time {
	switch {
	case s.State != Exhausted:
		return time.Time{}
	case !s.Resets.IsZero():
		return s.Resets
	case s.At.IsZero():
		return time.Time{}
	}
	return s.At.Add(ttl(s.Code, s.Reason, sole))
}

// When the model may be used with the account again, zero when nothing holds it back; for no model, the latest of every model held back
func (s Status) Cooled(model string) time.Time {
	var latest time.Time
	for name, until := range s.Cooldowns {
		if (model == "" || name == model) && time.Now().Before(until) && until.After(latest) {
			latest = until
		}
	}
	return latest
}

// States an account lists with
const (
	Ready       = "ready"
	Limited     = "limited"
	NeedsSignIn = "needs_sign_in"
	Gone        = "dead"
	Unavailable = "unavailable"
)

// How an account lists: ready, limited until a time, waiting to be signed in again, gone for good, or without a credential to use
func (r *Registry) State(account Account) (string, time.Time) {
	credential, ok := r.Credential(account)
	if !ok || credential.APIKey == "" && credential.Sign == nil {
		return Unavailable, time.Time{}
	}
	status := StatusOf(account)
	oauth := credential.Auth == provider.AuthOAuth
	if status.State == Dead {
		if oauth {
			return NeedsSignIn, time.Time{}
		}
		return Gone, time.Time{}
	}
	if until := status.Until(r.sole(account.Provider)); time.Now().Before(until) {
		if status.Code == 401 && oauth {
			return NeedsSignIn, until
		}
		return Limited, until
	}
	if until := status.Cooled(""); !until.IsZero() {
		return Limited, until
	}
	return Ready, time.Time{}
}

// Whether the provider has at most one account whose credential may still work, which then has nothing to rotate to
func (r *Registry) sole(instance provider.Instance) bool {
	live := 0
	for _, name := range instance.Accounts {
		if StatusOf(Of(instance, name)).State != Dead {
			live++
		}
	}
	return live <= 1
}

// Clears what stands against the account: its limit and its held back models, the reset another process sees too. Whether there was any
func Reset(account Account) (bool, error) {
	status := StatusOf(account)
	if status.State == "" && len(status.Cooldowns) == 0 {
		return false, nil
	}
	return true, Status{}.save(account)
}

// Accounts the profile's choice puts first, and the ones a provider held to one account moves on to when it may
func rows(instance provider.Instance) (first, spare []string) {
	var rotation, others []string
	for _, name := range instance.Accounts {
		marked := instance.AccountFields(name).GetBool(providerconfig.InRotation)
		switch {
		case instance.Pinned && name == instance.Account, !instance.Pinned && !instance.Rotate, instance.Rotate && marked:
			first = append(first, name)
		case marked:
			rotation = append(rotation, name)
		default:
			others = append(others, name)
		}
	}
	if !instance.Pinned || instance.Settings.GetString(providerconfig.OnExhausted) != providerconfig.Rotate {
		return first, nil
	}
	if len(rotation) > 0 {
		return first, rotation
	}
	return first, others
}

// Why no account could be used
type blocked struct {
	account string
	status  Status
	until   time.Time
}

// Account a request for the model goes out with, by the profile's choice among the provider's accounts and its rotation strategy, and
// its credential. Accounts named in skip are left out
func (r *Registry) choose(instance provider.Instance, model string, skip ...string) (Account, Credential, error) {
	if len(instance.Accounts) == 0 {
		account := Of(instance, "")
		r.renewDue(account)
		credential, ok := r.Credential(account)
		if !ok && !instance.Spec.Local && instance.Spec.Auth != provider.AuthNone {
			return account, credential, refusal(instance, nil, r.why(account))
		}
		return account, credential, nil
	}
	first, spare := rows(instance)
	sole := r.sole(instance)
	var stopped []blocked
	for _, names := range [][]string{first, spare} {
		var usable []Account
		credentials := map[string]Credential{}
		for _, name := range names {
			if slices.Contains(skip, name) {
				continue
			}
			account := Of(instance, name)
			credential, ok, why := r.usable(account, model, sole)
			if why != nil {
				stopped = append(stopped, *why)
			}
			if ok {
				usable = append(usable, account)
				credentials[name] = credential
			}
		}
		if len(usable) > 0 {
			account := pick(instance, usable)
			return account, credentials[account.Name], nil
		}
	}
	reason := ""
	if len(stopped) == 0 {
		for _, name := range slices.Concat(first, spare) {
			if !slices.Contains(skip, name) {
				reason = r.why(Of(instance, name))
				break
			}
		}
	}
	return Account{}, Credential{}, refusal(instance, stopped, reason)
}

// Whether the account can take a request for the model now, with its credential renewed when due; what stops it when something does
func (r *Registry) usable(account Account, model string, sole bool) (Credential, bool, *blocked) {
	r.renewDue(account)
	credential, ok := r.Credential(account)
	if !ok || credential.APIKey == "" && credential.Sign == nil {
		return credential, false, nil
	}
	status := StatusOf(account)
	if status.State == Dead {
		return credential, false, &blocked{account: account.Name, status: status}
	}
	if until := status.Cooled(model); model != "" && !until.IsZero() {
		return credential, false, &blocked{account: account.Name, status: status, until: until}
	}
	if until := status.Until(sole); !until.IsZero() {
		if time.Now().Before(until) && !r.early(account) {
			return credential, false, &blocked{account: account.Name, status: status, until: until}
		}
		status.State, status.At, status.Code, status.Reason, status.Message, status.Resets = "", time.Time{}, 0, "", "", time.Time{}
		if err := status.save(account); err != nil {
			slog.Warn("An account's lifted limit could not be cleared", "account", account.Fields.Prefix(), "error", err)
		}
	}
	return credential, true, nil
}

// One of the usable accounts by the provider's rotation strategy, in the order they are tried
func pick(instance provider.Instance, usable []Account) Account {
	switch instance.Settings.GetString(providerconfig.Strategy) {
	case providerconfig.Random:
		return usable[rand.IntN(len(usable))]
	case providerconfig.LeastUsed:
		return slices.MinFunc(usable, func(a, b Account) int {
			return cmp.Compare(a.Fields.GetInt(providerconfig.Requests), b.Fields.GetInt(providerconfig.Requests))
		})
	case providerconfig.RoundRobin:
		return slices.MinFunc(usable, func(a, b Account) int {
			return cmp.Compare(cast.ToInt64(a.Fields.Get(providerconfig.Used)), cast.ToInt64(b.Fields.Get(providerconfig.Used)))
		})
	}
	return usable[0]
}

// Error naming why no account of the provider can be used now and what to do about it
func refusal(instance provider.Instance, stopped []blocked, reason string) error {
	label := instance.Label()
	if len(stopped) == 0 && reason != "" {
		return fmt.Errorf("%s is not connected: %s", label, reason)
	}
	if len(stopped) == 0 {
		return fmt.Errorf("%s is not connected. Connect it with /providers.", label)
	}
	why := func(b blocked) string {
		return cmp.Or(b.status.Reason, b.status.Message, statusText(b.status.Code))
	}
	if len(stopped) == 1 && instance.Pinned {
		b := stopped[0]
		if b.status.State == Dead {
			return fmt.Errorf("The sign-in of the account %s of %s no longer works (%s). Sign in again with /providers or `icebath auth add %s`.",
				b.account, label, why(b), instance.ID)
		}
		return fmt.Errorf("The account %s of %s reached its limit (%s) until %s. Pick another account with /providers or `icebath auth use %s <account>`, "+
			"or let it move on to the others with `icebath auth rotation %s --on-exhausted rotate`.", b.account, label, why(b), b.until.Local().Format("15:04"), instance.ID, instance.ID)
	}
	var next time.Time
	for _, b := range stopped {
		if !b.until.IsZero() && (next.IsZero() || b.until.Before(next)) {
			next = b.until
		}
	}
	if next.IsZero() {
		return fmt.Errorf("No account of %s can be used: each sign-in was refused for good. Sign in again with /providers or `icebath auth add %s`.", label, instance.ID)
	}
	return fmt.Errorf("Every account of %s is at its limit; the first frees up at %s. Add another with /providers, or clear the limits with `icebath auth reset %s`.",
		label, next.Local().Format("15:04"), instance.ID)
}

func statusText(code int) string {
	if code == 0 {
		return "refused"
	}
	return "HTTP " + strconv.Itoa(code)
}

// Renews the account's credential when its registrant says it is about to expire, keeping a failure on the account
func (r *Registry) renewDue(account Account) {
	if !r.Due(account) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), renewWait)
	defer cancel()
	if err := r.Refresh(ctx, account, false); err != nil {
		slog.Warn("A sign-in could not be renewed", "provider", account.Provider.ID, "account", account.Name, "error", err)
	}
}

func (r *Registry) Due(account Account) bool {
	for _, registrant := range r.registrants {
		if renewer, ok := registrant.(Renewer); ok && renewer.Due(account) {
			return true
		}
	}
	return false
}

func (r *Registry) Renew(ctx context.Context, account Account) (bool, error) {
	for _, registrant := range r.registrants {
		if renewer, ok := registrant.(Renewer); ok {
			if renewed, err := renewer.Renew(ctx, account); renewed || err != nil {
				return renewed, err
			}
		}
	}
	return false, nil
}

func (r *Registry) Stale(account Account, failure *provider.APIError) bool {
	for _, registrant := range r.registrants {
		if staler, ok := registrant.(Staler); ok && staler.Stale(account, failure) {
			return true
		}
	}
	return false
}

// Whether the account's limit lifted before its time, as its registrant can tell
func (r *Registry) early(account Account) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return r.Restored(ctx, account)
}

func (r *Registry) Restored(ctx context.Context, account Account) bool {
	for _, registrant := range r.registrants {
		if restorer, ok := registrant.(Restorer); ok && restorer.Restored(ctx, account) {
			return true
		}
	}
	return false
}

// Error of an account whose credential nothing renews
var ErrNotRenewable = errors.New("Nothing renews this credential: it is a key, or a sign-in that keeps no refresh token.")

// Renews the account's credential when it is due, or now when forced, one renewal at a time across processes. What another process
// renewed meanwhile is taken instead of renewing again. A refusal that only signing in again fixes leaves the account dead
func (r *Registry) Refresh(ctx context.Context, account Account, force bool) error {
	release, err := r.pool.hold(ctx, account)
	if err != nil {
		return err
	}
	defer release()
	before, _ := r.Credential(account)
	if r.pool != nil && r.pool.config != nil {
		if err := r.pool.config.Load(); err != nil {
			return err
		}
	}
	after, _ := r.Credential(account)
	if !r.Due(account) && (!force || after.APIKey != before.APIKey) {
		return nil
	}
	renewed, err := r.Renew(ctx, account)
	switch {
	case err != nil:
		if failure, ok := errors.AsType[*AuthError](err); ok && failure.Relogin && account.Name != "" {
			status := StatusOf(account)
			status.State, status.At, status.Reason, status.Message = Dead, time.Now(), cmp.Or(failure.Code, "invalid_grant"), failure.Message
			if saved := status.save(account); saved != nil {
				return errors.Join(err, saved)
			}
		}
		return err
	case !renewed:
		return ErrNotRenewable
	}
	if account.Name == "" {
		return nil
	}
	// A renewed sign-in clears the limit its old token ran into
	status := StatusOf(account)
	if status.State == Dead || status.Code == 401 {
		status.State, status.At, status.Code, status.Reason, status.Message, status.Resets = "", time.Time{}, 0, "", "", time.Time{}
		return status.save(account)
	}
	return nil
}

// What one run of requests learned of its refusals: whether a limit was already given one more try, and the renewals of each account
type episode struct {
	mu       sync.Mutex
	retried  bool
	renewals map[string]int
}

// Route of a request to the provider with the account the profile's choice and rotation give for the model, renewed when due, and a
// keeper that renews it or moves on to the next account when the provider refuses it
func (r *Registry) Route(instance provider.Instance, model string) (provider.Route, error) {
	route := base(instance, model)
	if !instance.Known {
		return route, fmt.Errorf("%s is a kind of provider icebath doesn't know (%s).", instance.Label(), instance.Spec.Name)
	}
	account, credential, err := r.choose(instance, route.Model)
	if err != nil {
		return route, err
	}
	return r.routed(instance, route, account, credential, &episode{renewals: map[string]int{}})
}

// Route of a request to the provider with one account, held to it whatever the profile's choice, for a session pinned to it
func (r *Registry) RouteWith(instance provider.Instance, model, name string) (provider.Route, error) {
	route := base(instance, model)
	if !instance.Known {
		return route, fmt.Errorf("%s is a kind of provider icebath doesn't know (%s).", instance.Label(), instance.Spec.Name)
	}
	if !slices.Contains(instance.Accounts, name) {
		return route, fmt.Errorf("%s has no account named %s.", instance.Label(), name)
	}
	pinned := instance
	pinned.Account, pinned.Pinned, pinned.Rotate = name, true, false
	account, credential, err := r.choose(pinned, route.Model)
	if err != nil {
		return route, err
	}
	return r.routed(pinned, route, account, credential, &episode{renewals: map[string]int{}})
}

// Route of the kind as the provider has it, before any credential, its model named as the kind names it
func base(instance provider.Instance, model string) provider.Route {
	spec := instance.Spec
	return provider.Route{Provider: instance.ID, Spec: spec.Name, Model: spec.NativeModel(cmp.Or(model, spec.DefaultModel)), BaseURL: spec.BaseURL,
		Headers: spec.Headers, Mode: spec.Mode, Auth: spec.Auth, Local: spec.Local}
}

// The route with the account's credential and the keeper of both
func (r *Registry) routed(instance provider.Instance, route provider.Route, account Account, credential Credential, ep *episode) (provider.Route, error) {
	route.APIKey = credential.APIKey
	route.BaseURL = cmp.Or(credential.BaseURL, route.BaseURL)
	route.Auth = cmp.Or(credential.Auth, route.Auth)
	route.Account = account.Name
	route.Sign = credential.Sign
	route.Region = credential.Region
	if len(credential.Headers) > 0 {
		headers := map[string]string{}
		for name, value := range route.Headers {
			headers[name] = value
		}
		for name, value := range credential.Headers {
			headers[name] = value
		}
		route.Headers = headers
	}
	route.Mode = mode(instance, route.Model, route.Mode)
	if instance.Spec.Route != nil {
		route = instance.Spec.Route(route, instance.Settings)
	}
	if route.BaseURL == "" {
		return route, fmt.Errorf("%s has no base URL.", instance.Label())
	}
	if credential.APIKey != "" || credential.Sign != nil {
		model := route.Model
		route.Keeper = &provider.Keeper{
			Sent: func() { r.sent(instance, account) },
			Lifted: func() {
				if _, err := Reset(account); err != nil {
					slog.Warn("An account's lifted limit could not be cleared", "account", account.Fields.Prefix(), "error", err)
				}
			},
			Recover: func(ctx context.Context, failure *provider.APIError) (provider.Route, bool) {
				return r.recover(ctx, instance, model, account, ep, failure)
			},
		}
	}
	return route, nil
}

// API a request for the model takes: the one the provider's settings name, unless that is Responses for a model its kind serves only over
// chat completions; else the kind's for the model, else the route's
func mode(instance provider.Instance, model string, current provider.APIMode) provider.APIMode {
	var perModel provider.APIMode
	if instance.Spec.PerModelMode != nil && model != "" {
		perModel = instance.Spec.PerModelMode(model)
	}
	if configured := instance.Settings.GetString(providerconfig.APIMode); provider.KnownMode(configured) {
		if provider.APIMode(configured) == provider.ModeCodexResponses && perModel == provider.ModeChatCompletions {
			return provider.ModeChatCompletions
		}
		return provider.APIMode(configured)
	}
	return cmp.Or(perModel, current)
}

// Counts a request that went out with the account, for the strategies that pick by use
func (r *Registry) sent(instance provider.Instance, account Account) {
	if account.Name == "" {
		return
	}
	var err error
	switch instance.Settings.GetString(providerconfig.Strategy) {
	case providerconfig.LeastUsed:
		err = account.Fields.Set(providerconfig.Requests, account.Fields.GetInt(providerconfig.Requests)+1)
	case providerconfig.RoundRobin:
		err = account.Fields.Set(providerconfig.Used, time.Now().UnixMilli())
	}
	if err != nil {
		slog.Warn("A request could not be counted on its account", "account", account.Fields.Prefix(), "error", err)
	}
}

// Route to send a refused request again with: the account's credential renewed after a 401 or a stale credential, else the next account
// after its limit; false when nothing would do better
func (r *Registry) recover(ctx context.Context, instance provider.Instance, model string, account Account, ep *episode, failure *provider.APIError) (provider.Route, bool) {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	text := strings.ToLower(failure.Message)
	status := failure.StatusCode
	// A plugin's classification decides whether the account is rotated past
	if failure.Rotate != nil {
		if !*failure.Rotate || account.Name == "" {
			return provider.Route{}, false
		}
		r.exhaust(instance, account, failure)
		return r.next(instance, model, account, ep)
	}
	switch {
	case status == 401 || status == 403 || failure.Kind == provider.ErrAuth:
		if entitlement(instance, status, text) {
			slog.Info("An entitlement refusal can't be fixed by renewing the sign-in", "provider", instance.ID, "status", status)
			return provider.Route{}, false
		}
		if route, ok := r.renewed(ctx, instance, model, account, ep); ok {
			return route, true
		}
		if account.Name == "" {
			return provider.Route{}, false
		}
		r.exhaust(instance, account, failure)
		return r.next(instance, model, account, ep)
	case status == 402:
		if account.Name == "" {
			return provider.Route{}, false
		}
		r.exhaust(instance, account, failure)
		return r.next(instance, model, account, ep)
	case status == 400 && strings.Contains(text, entitlementMarker):
		if account.Name == "" || model == "" {
			return provider.Route{}, false
		}
		r.cool(account, model, time.Now().Add(ttlEntitlement))
		return r.next(instance, model, account, ep)
	case status == 429 || failure.Kind == provider.ErrRateLimit:
		if account.Name == "" || instance.Spec.Aggregator && (strings.Contains(text, "upstream") || strings.Contains(text, "provider_name")) {
			return provider.Route{}, false
		}
		// Anthropic limits one model of an account at a time, so only that model waits
		if instance.Spec.Mode == provider.ModeAnthropicMessages && model != "" && !strings.Contains(text, "billing") && !strings.Contains(text, "credit") {
			r.cool(account, model, resets(failure, time.Now().Add(ttl(429, "", r.sole(instance)))))
			return r.next(instance, model, account, ep)
		}
		usage := strings.Contains(text, "usage_limit_reached") || strings.Contains(text, "gousagelimit") || strings.Contains(text, "usage limit")
		if StatusOf(account).State != Exhausted && !usage && !ep.retried {
			ep.retried = true
			return provider.Route{}, false
		}
		r.exhaust(instance, account, failure)
		return r.next(instance, model, account, ep)
	case r.Stale(account, failure):
		return r.renewed(ctx, instance, model, account, ep)
	}
	return provider.Route{}, false
}

// The account's route with its credential renewed, at most twice in a run of refusals
func (r *Registry) renewed(ctx context.Context, instance provider.Instance, model string, account Account, ep *episode) (provider.Route, bool) {
	if ep.renewals[account.Name] >= maxRenewals {
		slog.Warn("A sign-in is still refused after renewing it; the failure is left to the fallbacks", "provider", instance.ID, "account", account.Name)
		return provider.Route{}, false
	}
	ep.renewals[account.Name]++
	if err := r.Refresh(ctx, account, true); err != nil {
		if !errors.Is(err, ErrNotRenewable) {
			slog.Warn("A refused sign-in could not be renewed", "provider", instance.ID, "account", account.Name, "error", err)
		}
		return provider.Route{}, false
	}
	credential, ok := r.Credential(account)
	if !ok {
		return provider.Route{}, false
	}
	route, err := r.routed(instance, base(instance, model), account, credential, ep)
	return route, err == nil
}

// Route with the next account the provider may take for the model, leaving out the one that failed
func (r *Registry) next(instance provider.Instance, model string, failed Account, ep *episode) (provider.Route, bool) {
	account, credential, err := r.choose(instance, model, failed.Name)
	if err != nil || account.Name == failed.Name {
		return provider.Route{}, false
	}
	slog.Info("Moved on to the next account", "provider", instance.ID, "from", failed.Name, "to", account.Name)
	route, err := r.routed(instance, base(instance, model), account, credential, ep)
	return route, err == nil
}

// Whether a refusal says the account lacks a subscription or permission that no renewal brings back
func entitlement(instance provider.Instance, status int, text string) bool {
	if strings.Contains(text, "[wke=unauthenticated:") || strings.Contains(text, "oauth2 access token could not be validated") {
		return false
	}
	if strings.Contains(text, "do not have an active grok subscription") || strings.Contains(text, "grok") &&
		(strings.Contains(text, "out of available resources") || strings.Contains(text, "does not have permission")) {
		return true
	}
	if status != 403 {
		return false
	}
	return strings.Contains(text, "oauth authentication is currently not allowed for this organization") ||
		instance.Spec.Mode == provider.ModeAnthropicMessages && instance.Spec.Name == "anthropic" || instance.Spec.Name == "xai-oauth"
}

// When a refusal says its limit lifts, else the fallback
func resets(failure *provider.APIError, fallback time.Time) time.Time {
	if failure.RetryAfter > 0 {
		return time.Now().Add(failure.RetryAfter)
	}
	if delay, ok := Delay(failure.Message); ok {
		return time.Now().Add(delay)
	}
	return fallback
}

// Wait a refusal's text names before its limit lifts, false when it names none
func Delay(message string) (time.Duration, bool) {
	seconds := func(text string) float64 { n, _ := strconv.ParseFloat(text, 64); return n }
	if m := quotaDelay.FindStringSubmatch(message); m != nil {
		v := seconds(m[1])
		if strings.EqualFold(m[2], "ms") {
			v /= 1000
		}
		return time.Duration(v * float64(time.Second)), true
	}
	if m := retryAfter.FindStringSubmatch(message); m != nil {
		return time.Duration(seconds(m[1]) * float64(time.Second)), true
	}
	if m := resetsSeconds.FindStringSubmatch(message); m != nil {
		return time.Duration(seconds(m[1]) * float64(time.Second)), true
	}
	if m := resetsIn.FindStringSubmatch(message); m != nil && (m[1] != "" || m[2] != "" || m[3] != "") {
		return time.Duration((seconds(m[1])*3600 + seconds(m[2])*60 + seconds(m[3])) * float64(time.Second)), true
	}
	return 0, false
}

// Benches the account after a refusal: dead when the refusal is for good, else exhausted until its limit lifts
func (r *Registry) exhaust(instance provider.Instance, account Account, failure *provider.APIError) {
	status := StatusOf(account)
	reason := reasonOf(failure)
	status.State, status.At, status.Code, status.Reason, status.Message = Exhausted, time.Now(), failure.StatusCode, reason, failure.Message
	status.Resets = resets(failure, time.Time{})
	if failure.StatusCode == 401 && terminal[reason] {
		status.State = Dead
		slog.Warn("An account's sign-in was refused for good; it leaves the rotation until signed in again", "provider", instance.ID, "account", account.Name, "reason", reason)
	} else {
		slog.Info("An account reached its limit; moving on", "provider", instance.ID, "account", account.Name, "status", failure.StatusCode)
	}
	if err := status.save(account); err != nil {
		slog.Warn("An account's limit could not be kept", "account", account.Fields.Prefix(), "error", err)
	}
}

// Holds the model back on the account until the time given
func (r *Registry) cool(account Account, model string, until time.Time) {
	status := StatusOf(account)
	status.Cooldowns[model] = until
	if err := status.save(account); err != nil {
		slog.Warn("A model's limit could not be kept on its account", "account", account.Fields.Prefix(), "error", err)
	}
}

// Error code a refusal names in its body, such as invalid_grant or rate_limit_exceeded
var codeField = regexp.MustCompile(`"(?:code|type|error)"\s*:\s*"([a-z_]+)"`)

func reasonOf(failure *provider.APIError) string {
	if m := codeField.FindStringSubmatch(failure.Message); m != nil {
		return m[1]
	}
	if failure.StatusCode == 402 {
		return "billing"
	}
	return string(failure.Kind)
}

// Holds an account of the provider for one stream of requests, such as a delegated agent's, so that parallel ones spread across the
// accounts: the one asked for, else the usable one least held. Its route and the releaser
func (r *Registry) Lease(instance provider.Instance, model, name string) (provider.Route, func(), error) {
	if name == "" && len(instance.Accounts) > 1 && r.pool != nil {
		r.pool.mu.Lock()
		held := map[string]int{}
		for _, account := range instance.Accounts {
			held[account] = r.pool.leases[key(Of(instance, account))]
		}
		r.pool.mu.Unlock()
		order := slices.Clone(instance.Accounts)
		slices.SortStableFunc(order, func(a, b string) int { return cmp.Compare(held[a], held[b]) })
		first, spare := rows(instance)
		for _, account := range order {
			if slices.Contains(first, account) || slices.Contains(spare, account) {
				if _, ok, _ := r.usable(Of(instance, account), model, r.sole(instance)); ok {
					name = account
					break
				}
			}
		}
	}
	var route provider.Route
	var err error
	if name == "" {
		route, err = r.Route(instance, model)
	} else {
		route, err = r.RouteWith(instance, model, name)
	}
	if err != nil || route.Account == "" || r.pool == nil {
		return route, func() {}, err
	}
	k := key(Of(instance, route.Account))
	r.pool.mu.Lock()
	r.pool.leases[k]++
	r.pool.mu.Unlock()
	var once sync.Once
	return route, func() {
		once.Do(func() {
			r.pool.mu.Lock()
			defer r.pool.mu.Unlock()
			if r.pool.leases[k]--; r.pool.leases[k] <= 0 {
				delete(r.pool.leases, k)
			}
		})
	}, nil
}
