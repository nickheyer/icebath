package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/vault"
)

var vaultToolLogger = slog.Default().With("logger", "tools.browser_vault_tool")

// The supervisor calls the vault tool makes on a task's CDP session
type vaultSupervisor interface {
	evaluateRuntime(expression string, returnByValue, awaitPromise bool, timeout float64) *pyjson.Object
	focusPage(origin, accept string, timeout float64) *pyjson.Object
}

// Module attributes of the vault tool hermes tests patch
var (
	vaultEvalJS            = evalVaultJS
	vaultEvalJSSecret      = evalVaultJSSecret
	vaultCurrentPageOrigin = currentVaultPageOrigin
	vaultFocusBoundOrigin  = focusVaultBoundOrigin
	vaultEnsureSupervisor  = ensureVaultSupervisor
	vaultCanPromptHere     = CanPromptVaultHere
	vaultElicitConsent     = tools.RequestElicitationConsent
	loginBackendForHandle  = LoginBackendForHandle
	vaultFillFn            = browserVaultFill
	vaultSessionForTask    = sessionForTask
	vaultStartSupervisor   = func(env *tools.Env, taskID, cdpURL, policy string, timeoutS float64) (vaultSupervisor, error) {
		sup, err := supervisors.getOrStart(env, taskID, cdpURL, policy, timeoutS, supervisorStartTimeoutS)
		if err != nil || sup == nil {
			return nil, err
		}
		return sup, nil
	}
	vaultRegisteredSup = func(taskID string) vaultSupervisor {
		if sup := supervisors.get(taskID); sup != nil {
			return sup
		}
		return nil
	}
)

const pageFillFailedKey = "icebath_vault_eval_failed"

func vaultDumps(v any) string { return jsonText(v) }

func vaultDumpsUnicode(v any) string {
	text, err := pyjson.Dumps(v, pyjson.EnsureASCII(false))
	if err != nil {
		panic(err)
	}
	return text
}

func vaultFail(pairs ...any) *pyjson.Object {
	return pyjson.ObjectOf(append([]any{"success", false}, pairs...)...)
}

// The plugin browser holding the task's live page, nil for the CDP session
func vaultPageBackend(ctx context.Context, taskID string) BrowserPageBackend {
	return backendForTask(ctx, taskID)
}

// Evaluates on a plugin page backend, every failure of the secret path answered with fixed text
func vaultPageEval(backend BrowserPageBackend, taskID, expression string, secret bool) *pyjson.Object {
	if secret {
		expression = "(() => { try { return (" + expression + "); } catch (_) { return " +
			vaultDumps(pyjson.ObjectOf(pageFillFailedKey, true)) + "; } })()"
	}
	result, err := func() (result any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return backend.Evaluate(taskID, expression, secret)
	}()
	if err != nil {
		detail := ""
		if !secret {
			detail = ": " + vaultHead(err.Error(), 200)
		}
		return vaultFail("error_type", "eval_failed", "error", "The "+backend.Name()+" browser could not run the vault script"+detail)
	}
	if secret {
		if parsed, ok := pyjson.AsObject(parseVaultJSON(result)); ok && parsed.Len() == 1 && pyrepr.Equal(anyGet(parsed, pageFillFailedKey), true) {
			return vaultFail("error_type", "eval_failed", "error", "The "+backend.Name()+" browser could not run the vault script")
		}
	}
	return pyjson.ObjectOf("success", true, "result", result)
}

// Evaluates inspection script on the page backend holding the task's page, else over its CDP supervisor
func evalVaultJS(ctx context.Context, env *tools.Env, taskID, expression string) *pyjson.Object {
	if page := vaultPageBackend(ctx, taskID); page != nil {
		return vaultPageEval(page, taskID, expression, false)
	}
	supervisor := vaultEnsureSupervisor(ctx, env, taskID)
	if supervisor == nil {
		return vaultFail("error", "no supervised browser session for this task; open the page with browser_exec first")
	}
	sup := supervisor.evaluateRuntime(expression, true, true, 10.0)
	if pyrepr.Truthy(anyGet(sup, "ok")) {
		return pyjson.ObjectOf("success", true, "result", anyGet(sup, "result"))
	}
	return vaultFail("error", pyrepr.Str(pyrepr.Or(anyGet(sup, "error"), "evaluate failed")))
}

// The task's supervisor, attached on demand to its session CDP endpoint, nil when no endpoint is known
func ensureVaultSupervisor(ctx context.Context, env *tools.Env, taskID string) vaultSupervisor {
	if sup := vaultRegisteredSup(taskID); sup != nil {
		return sup
	}
	cdpURL := vaultText(vaultSessionForTask(taskID)["cdp_url"])
	if cdpURL == "" {
		return nil
	}
	policy, timeoutS := cfgOf(env).dialogPolicyConfig()
	sup, err := vaultStartSupervisor(env, taskID, ResolveCDPOverride(ctx, cdpURL), policy, timeoutS)
	if err != nil || sup == nil {
		vaultToolLogger.Debug(fmt.Sprintf("vault fill: supervisor attach to the task's session failed (%v)", err))
		return nil
	}
	return sup
}

func vaultBotDesktopSession(env *tools.Env, taskID string) bool {
	return sharesBotDesktopBrowser(env, vaultSessionForTask(taskID))
}

// Evaluates a secret bearing script over the supervisor socket or the page backend, never through argv
func evalVaultJSSecret(ctx context.Context, env *tools.Env, taskID, expression string) *pyjson.Object {
	page := vaultPageBackend(ctx, taskID)
	var supervisor vaultSupervisor
	if page == nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					vaultToolLogger.Debug(fmt.Sprintf("vault fill: supervisor unavailable (%v)", r))
					supervisor = nil
				}
			}()
			supervisor = vaultEnsureSupervisor(ctx, env, taskID)
		}()
	}
	if page == nil && supervisor == nil {
		return vaultFail("error_type", "supervisor_required", "error",
			"Vault fill requires the supervised browser session (direct "+
				"CDP WebSocket), and this task has none. Open the page with "+
				"browser_exec and retry.")
	}
	if vaultBotDesktopSession(env, taskID) {
		if _, err := botDesktopAssertAgentMayAct(env); err != nil {
			return vaultFail("error_type", "human_has_control", "error", err.Error())
		}
	}
	if page != nil {
		return vaultPageEval(page, taskID, expression, true)
	}
	sup := supervisor.evaluateRuntime(expression, true, true, 10.0)
	if pyrepr.Truthy(anyGet(sup, "ok")) {
		return pyjson.ObjectOf("success", true, "result", anyGet(sup, "result"))
	}
	errorType := "eval_failed"
	if strings.Contains(strings.ToLower(vaultText(anyGet(sup, "error"))), "supervisor") {
		errorType = "supervisor_required"
	}
	return vaultFail("error_type", errorType, "error", pyrepr.Str(pyrepr.Or(anyGet(sup, "error"), "eval failed")))
}

// JSON text decoded to its value, anything that is not JSON text returned as is
func parseVaultJSON(raw any) any {
	text, ok := raw.(string)
	if !ok {
		return raw
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return raw
	}
	return parsed
}

func parseVaultJSONTwice(raw any) any {
	parsed := parseVaultJSON(raw)
	if _, ok := parsed.(string); ok {
		parsed = parseVaultJSON(parsed)
	}
	return parsed
}

// Normalized origin of the current page, empty for a blank page or an unreadable one
func currentVaultPageOrigin(ctx context.Context, env *tools.Env, taskID string) string {
	res := vaultEvalJS(ctx, env, taskID, "window.location.href")
	if !pyrepr.Truthy(anyGet(res, "success")) {
		return ""
	}
	href := pystr.StripChars(pystr.StripChars(pystr.Strip(vaultText(anyGet(res, "result"))), `"`), "'")
	if href == "" || href == "about:blank" {
		return ""
	}
	origin, err := vault.NormalizeOrigin(href)
	if err != nil {
		return ""
	}
	return origin
}

// Script truthy when the selector matches in the document or any open shadow root
func deepProbe(selector string) string {
	return "((s) => { const w = (r) => r.querySelector(s) || Array.from(r.querySelectorAll('*'))" +
		".some((e) => e.shadowRoot && w(e.shadowRoot)); return !!w(document); })(" + vaultDumps(selector) + ")"
}

var vaultTabProbes = map[string]string{
	"login":   deepProbe("input[type=password]"),
	"payment": deepProbe("input[autocomplete^=cc-], [name*=card i], [placeholder*=card i], [name*=cvc i], [name*=cvv i]"),
	"address": deepProbe("input[autocomplete^=address-], [autocomplete=postal-code], [name*=address i], [name*=zip i], [name*=postal i]"),
	"otp": deepProbe("input[autocomplete=one-time-code], input[name*=otp i], input[name*=code i], " +
		"input[id*=otp i], input[id*=code i], input[name*=totp i], input[aria-label*=code i]"),
}

// Points the supervisor at the open tab on origin holding a form of the kind, empty when none was focused
func focusVaultBoundOrigin(ctx context.Context, env *tools.Env, taskID, origin, kind string) string {
	if vaultPageBackend(ctx, taskID) != nil {
		return ""
	}
	var supervisor vaultSupervisor
	func() {
		defer func() {
			if recover() != nil {
				supervisor = nil
			}
		}()
		supervisor = vaultEnsureSupervisor(ctx, env, taskID)
	}()
	if supervisor == nil {
		return ""
	}
	focused := supervisor.focusPage(origin, vaultTabProbes[kind], 10.0)
	if !pyrepr.Truthy(anyGet(focused, "ok")) {
		return ""
	}
	if origin != "" {
		return origin
	}
	return vaultText(anyGet(focused, "url"))
}

// The exact origins a vault item may be filled on
func savedOrigins(meta *vault.VaultItemMeta) []string {
	if len(meta.AllowedOrigins) > 0 {
		return append([]string(nil), meta.AllowedOrigins...)
	}
	if pyrepr.Truthy(meta.Origin) {
		return []string{pyrepr.Str(meta.Origin)}
	}
	return []string{}
}

// The page origin when it is one of the item's saved origins, else the refusal to return
func boundVaultPageOrigin(ctx context.Context, env *tools.Env, taskID string, meta *vault.VaultItemMeta, probeKind string) (string, *pyjson.Object) {
	allowed := savedOrigins(meta)
	if len(allowed) == 0 {
		return "", vaultFail("error_type", "no_origin", "error", "Vault item "+pyrepr.Repr(meta.ID)+" has no bound origin; "+meta.Kind+
			" items are filled only on the site they were saved for.")
	}
	pageOrigin := ""
	for _, candidate := range allowed {
		if o := vaultFocusBoundOrigin(ctx, env, taskID, candidate, probeKind); o != "" {
			pageOrigin = o
			break
		}
	}
	if pageOrigin == "" {
		pageOrigin = vaultCurrentPageOrigin(ctx, env, taskID)
	}
	if pageOrigin == "" {
		return "", vaultFail("error", "Could not determine the current page origin. Navigate to the login page first.")
	}
	if !hasToken(allowed, pageOrigin) {
		return "", vaultFail("error_type", "origin_mismatch", "error", "Refused: current page origin ("+pageOrigin+
			") does not match the vault item's bound origin(s) ("+strings.Join(allowed, ", ")+
			"). Vault fills only run on the exact origin(s) the credential was saved for.")
	}
	return pageOrigin, nil
}

// Lists handles and metadata across every enabled backend, a locked manager reported under locked
func browserVaultList(ctx context.Context, env *tools.Env) (string, error) {
	backends, err := enabledLoginBackendsFn(ctx)
	if err != nil {
		return "", err
	}
	items, locked, failures := []any{}, []any{}, []any{}
	for _, backend := range backends {
		if backend.NeedsUnlock() && !backend.IsUnlocked() {
			unlock := "unavailable_in_this_session"
			if vaultCanPromptHere(ctx, env) {
				unlock = "browser_vault action=unlock"
			}
			locked = append(locked, pyjson.ObjectOf("backend", backend.Name(), "display_name", backend.DisplayName(), "unlock", unlock))
			continue
		}
		metas, err := backend.ListItems()
		if err != nil {
			failures = append(failures, pyjson.ObjectOf("backend", backend.Name(), "error", vaultHead(err.Error(), 200)))
			continue
		}
		for _, meta := range metas {
			entry := pyjson.ObjectOf("handle", meta.ID, "backend", backend.Name(), "label", meta.Label, "kind", meta.Kind,
				"origin", meta.Origin, "available", meta.Kind == "login" || pyrepr.Truthy(meta.Origin))
			if len(meta.AllowedOrigins) > 1 {
				origins := make([]any, 0, len(meta.AllowedOrigins))
				for _, o := range meta.AllowedOrigins {
					origins = append(origins, o)
				}
				entry.Set("allowed_origins", origins)
			}
			if meta.HasOTP {
				entry.Set("two_factor", "automatic")
			} else if backend.NeedsUnlock() {
				entry.Set("two_factor", "automatic if the manager stores a TOTP seed, else the user is asked")
			}
			if pyrepr.Truthy(meta.Identifier) {
				entry.Set("identifier", meta.Identifier)
				entry.Set("identifier_type", meta.IdentifierType)
			}
			items = append(items, entry)
		}
	}
	out := pyjson.ObjectOf("success", true, "items", items)
	if len(items) == 0 {
		out.Set("hint", "No saved logins. On a login page, call browser_vault action=save_login to ask the user to save one. "+
			"Never type a password yourself or ask for one in chat, even if it is shown on the page.")
	}
	if len(locked) > 0 {
		out.Set("locked", locked)
	}
	if len(failures) > 0 {
		out.Set("errors", failures)
	}
	return vaultDumpsUnicode(out), nil
}

// Asks the user through the surface's masked prompt to unlock an external manager for this session
func browserVaultUnlock(ctx context.Context, env *tools.Env, backendName string) (string, error) {
	backends, err := enabledLoginBackendsFn(ctx)
	if err != nil {
		return "", err
	}
	var backend LoginBackend
	for _, b := range backends {
		if b.Name() == backendName && b.NeedsUnlock() {
			backend = b
			break
		}
	}
	if backend == nil {
		return vaultDumps(vaultFail("error", "No unlockable vault backend named "+pyrepr.Repr(backendName)+".")), nil
	}
	if backend.IsUnlocked() {
		return vaultDumps(pyjson.ObjectOf("success", true, "backend", backend.Name(), "already_unlocked", true)), nil
	}
	if !vaultCanPromptHere(ctx, env) {
		return vaultDumps(vaultFail("error_type", "unlock_unavailable", "error", backend.DisplayName()+
			" is locked and this session cannot prompt for the "+
			"master password (headless/cron/API). Unlock it from an interactive Icebath "+
			"session or the Desktop app first.")), nil
	}
	master := ""
	if prompt := GetUnlockPromptCallback(ctx); prompt != nil {
		master = prompt(backend.Name(), backend.DisplayName())
	}
	if master == "" {
		return vaultDumps(vaultFail("error_type", "unlock_cancelled", "error", "The user declined to unlock "+backend.DisplayName()+".")), nil
	}
	unlockable, ok := backend.(unlockableLoginBackend)
	if !ok {
		err = fmt.Errorf("'%s' object has no attribute 'unlock'", pyrepr.TypeName(backend))
	} else {
		err = unlockable.Unlock(master)
	}
	if err != nil {
		return vaultDumps(vaultFail("error_type", "unlock_failed", "error", vaultHead(err.Error(), 300))), nil
	}
	return vaultDumps(pyjson.ObjectOf("success", true, "backend", backend.Name())), nil
}

func originHost(origin string) string {
	if _, host, found := strings.Cut(origin, "://"); found {
		return host
	}
	return origin
}

// Asks the user for the current page login, stores it bound to that origin and fills the password
func browserVaultSaveLogin(ctx context.Context, env *tools.Env, label, taskID string) (string, error) {
	if taskID == "" {
		taskID = "default"
	}
	vaultFocusBoundOrigin(ctx, env, taskID, "", "login")
	origin := vaultCurrentPageOrigin(ctx, env, taskID)
	if origin == "" {
		return vaultDumps(vaultFail("error", "Open the site's login page first; the login is saved for that page's origin.")), nil
	}
	prompt := GetSaveLoginPromptCallback(ctx)
	if prompt == nil || !vaultCanPromptHere(ctx, env) {
		return vaultDumps(vaultFail("error_type", "prompt_unavailable", "error",
			"This session cannot ask the user for a login (headless/cron/API). Tell them to run "+
				"`icebath vault add` or use Desktop → Settings → Passwords & Logins for "+origin+".")), nil
	}
	host := originHost(origin)
	site := pystr.Strip(label)
	if site == "" {
		site = host
	}
	answer := prompt(origin, host)
	if len(answer) == 0 || answer["password"] == "" || answer["identifier"] == "" {
		return vaultDumps(vaultFail("error_type", "save_declined", "error", "The user chose not to save a login for this site. Do not ask again this turn.")), nil
	}
	identifier := pystr.Strip(answer["identifier"])
	idType := "username"
	if strings.Contains(identifier, "@") {
		idType = "email"
	} else if pystr.StrIsDigit(pystr.LstripChars(identifier, "+")) {
		idType = "phone"
	}
	meta, err := localVaultStore(ctx).AddItem("login", site, pyjson.ObjectOf("identifier_type", idType, "identifier", identifier,
		"password", answer["password"]), origin)
	clear(answer)
	if err != nil {
		return vaultDumps(vaultFail("error_type", "save_failed", "error", vaultHead(err.Error(), 200))), nil
	}
	fillText, err := vaultFillFn(ctx, env, meta.ID, taskID)
	if err != nil {
		return "", err
	}
	filled, err := pyjson.Loads(fillText)
	if err != nil {
		return "", err
	}
	return vaultDumpsUnicode(pyjson.ObjectOf("success", true, "handle", meta.ID, "origin", origin, "identifier", identifier,
		"identifier_type", idType, "fill", filled,
		"next", "Type the identifier into the username field if the form has one, then submit.")), nil
}

// Backend and metadata of a handle, no metadata when nothing is saved under it or its manager is locked
func savedVaultLogin(ctx context.Context, handle string) (LoginBackend, *vault.VaultItemMeta, error) {
	if handle == "" {
		return nil, nil, nil
	}
	backend, err := loginBackendForHandle(ctx, handle)
	if err != nil || backend == nil {
		return nil, nil, err
	}
	meta, err := backend.GetMeta(handle)
	var locked *UnlockRequired
	if errors.As(err, &locked) {
		return backend, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return backend, meta, nil
}

// A plugin backend failure anywhere in a vault action becomes the backend_unavailable refusal
func pluginErrorsAsRefusal(result string, err error) (string, error) {
	var pluginErr *PluginBackendError
	if errors.As(err, &pluginErr) {
		return vaultDumps(vaultFail("error_type", "backend_unavailable", "error", pluginErr.Error())), nil
	}
	return result, err
}

// Nil when the item still binds the page origin inside a backend's own fill context, else the refusal
func snapshotRefusal(backend LoginBackend, handle string, meta *vault.VaultItemMeta, pageOrigin string, ownContext bool) (*pyjson.Object, error) {
	if !ownContext {
		return nil, nil
	}
	fresh, err := backend.GetMeta(handle)
	if err != nil {
		return nil, err
	}
	if fresh != nil && fresh.Kind == meta.Kind && hasToken(savedOrigins(fresh), pageOrigin) {
		return nil, nil
	}
	return vaultFail("error_type", "origin_mismatch", "error", "Refused: the saved item changed before the fill and no longer binds "+pageOrigin+
		". Nothing was resolved or written."), nil
}

// Mints the backend's one time code and writes it inside the fill context, nil when the backend has no code
func writeBackendCode(backend LoginBackend, handle string, meta *vault.VaultItemMeta, origin string, write func(code string) string) (*string, error) {
	var written *string
	finish := func(result *string, err error) (*string, error) {
		var pluginErr *PluginBackendError
		if errors.As(err, &pluginErr) {
			return written, nil
		}
		return result, err
	}
	release, err := backend.FillContext()
	if err != nil {
		return finish(nil, err)
	}
	result, bodyErr := func() (*string, error) {
		refusal, err := snapshotRefusal(backend, handle, meta, origin, release != nil)
		if err != nil {
			return nil, err
		}
		if refusal != nil {
			text := vaultDumps(refusal)
			return &text, nil
		}
		code, present, err := backend.ResolveOTP(handle)
		if err == nil && present && code != "" {
			text := write(code)
			written = &text
		}
		return written, nil
	}()
	if release != nil {
		if exitErr := release(); exitErr != nil {
			return finish(nil, exitErr)
		}
	}
	return finish(result, bodyErr)
}

func vaultNonce() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)
}

// Controls of the inspection result that are objects, each built into its descriptor
func inspectedControls(raw any) ([]LoginControl, error) {
	out := []LoginControl{}
	if !pyrepr.Truthy(raw) {
		return out, nil
	}
	list, ok := raw.([]any)
	if !ok {
		if obj, isObj := pyjson.AsObject(raw); isObj && obj.Len() > 0 {
			return out, nil
		}
		if _, isStr := raw.(string); isStr {
			return out, nil
		}
		return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(raw))
	}
	for _, entry := range list {
		if obj, ok := pyjson.AsObject(entry); ok {
			control, err := LoginControlFromDict(obj)
			if err != nil {
				return nil, err
			}
			out = append(out, control)
		}
	}
	return out, nil
}

// Fills the one time code the current page asks for, minted from a saved login or typed by the user
func browserVaultEnterCode(ctx context.Context, env *tools.Env, handle, taskID string) (string, error) {
	return pluginErrorsAsRefusal(enterVaultCode(ctx, env, handle, taskID))
}

func enterVaultCode(ctx context.Context, env *tools.Env, handle, taskID string) (string, error) {
	if taskID == "" {
		taskID = "default"
	}
	backend, meta, err := savedVaultLogin(ctx, handle)
	if err != nil {
		return "", err
	}
	origin := ""
	if meta != nil {
		bound, refusal := boundVaultPageOrigin(ctx, env, taskID, meta, "otp")
		if refusal != nil {
			if anyGet(refusal, "error_type") == "origin_mismatch" {
				refusal.Set("next", "If this page asks for the code of a different account, call enter_code without a "+
					"handle so the user is asked.")
			}
			return vaultDumps(refusal), nil
		}
		origin = bound
	} else {
		vaultFocusBoundOrigin(ctx, env, taskID, "", "otp")
		origin = vaultCurrentPageOrigin(ctx, env, taskID)
		if origin == "" {
			return vaultDumps(vaultFail("error", "No page with a code field is open.")), nil
		}
	}
	site := originHost(origin)

	nonce := vaultNonce()
	inspect := vaultEvalJS(ctx, env, taskID, BuildInspectionJS(nonce))
	var rawControls any
	if pyrepr.Truthy(anyGet(inspect, "success")) {
		rawControls = parseVaultJSONTwice(anyGet(inspect, "result"))
	}
	controls, err := inspectedControls(rawControls)
	if err != nil {
		return "", err
	}
	otpControls := ClassifyOTPControls(controls)
	if len(otpControls) == 0 {
		return vaultDumps(vaultFail("error_type", "no_code_field", "error",
			"No one-time-code field on the current page. If the site wants a passkey, hardware key or "+
				"an approval tap in an app, tell the user to complete it on their device and wait for the page to move on.")), nil
	}

	write := func(code, source string) string {
		tools.RegisterVaultRedactionValueIn(ctx, code)
		fills := BuildOTPFills(otpControls, code)
		result := vaultEvalJSSecret(ctx, env, taskID, BuildFillJS(fills, origin, nonce))
		if !pyrepr.Truthy(anyGet(result, "success")) {
			return vaultDumps(vaultFail("error", vaultHead(pyrepr.Str(pyrepr.Or(anyGet(result, "error"), "fill failed")), 200)))
		}
		parsed, isObj := pyjson.AsObject(parseVaultJSONTwice(anyGet(result, "result")))
		if isObj && anyGet(parsed, "refused") == "origin_changed" {
			return vaultDumps(vaultFail("error_type", "origin_changed", "error", "The page navigated before the code could be entered. Nothing was written."))
		}
		var filled int64
		if isObj {
			if v, ok := parsed.Get("filled"); ok {
				n, err := pyint.Int64(v)
				if err != nil {
					panic(err)
				}
				filled = n
			}
		}
		return vaultDumps(pyjson.ObjectOf("success", filled != 0, "filled_fields", filled, "origin", origin, "source", source,
			"next", "Submit the form (many sites auto-submit when the last digit lands)."))
	}

	if meta != nil {
		written, err := writeBackendCode(backend, handle, meta, origin, func(code string) string { return write(code, backend.Name()) })
		if err != nil {
			return "", err
		}
		if written != nil {
			return *written, nil
		}
	}
	prompt := GetCodePromptCallback(ctx)
	if prompt == nil || !vaultCanPromptHere(ctx, env) {
		return vaultDumps(vaultFail("error_type", "prompt_unavailable", "error", site+
			" asks for a one-time code and this session cannot ask the user (headless/cron/API). "+
			"Save an authenticator key for this login so codes can be generated automatically.")), nil
	}
	code := strings.ReplaceAll(strings.ReplaceAll(pystr.Strip(prompt(site, "")), " ", ""), "-", "")
	if code == "" {
		return vaultDumps(vaultFail("error_type", "code_declined", "error", "The user did not enter a code. Do not ask again this turn.")), nil
	}
	return write(code, "user"), nil
}

// Fills the current page from a vault handle, the result reporting only counts and metadata
func browserVaultFill(ctx context.Context, env *tools.Env, handle, taskID string) (string, error) {
	return pluginErrorsAsRefusal(fillVaultHandle(ctx, env, handle, taskID))
}

func lockedAgain(backend LoginBackend) string {
	return vaultDumps(vaultFail("error_type", "unlock_required", "error", backend.DisplayName()+" locked again; call browser_vault action=unlock."))
}

func fillVaultHandle(ctx context.Context, env *tools.Env, handle, taskID string) (string, error) {
	if taskID == "" {
		taskID = "default"
	}
	backend, err := loginBackendForHandle(ctx, handle)
	if err != nil {
		return "", err
	}
	if backend != nil && backend.NeedsUnlock() && !backend.IsUnlocked() {
		unlockText, err := browserVaultUnlock(ctx, env, backend.Name())
		if err != nil {
			return "", err
		}
		unlocked, err := pyjson.Loads(unlockText)
		if err != nil {
			return "", err
		}
		if !pyrepr.Truthy(anyGet(unlocked, "success")) {
			return vaultDumps(unlocked), nil
		}
	}
	var meta *vault.VaultItemMeta
	if backend != nil {
		meta, err = backend.GetMeta(handle)
		var locked *UnlockRequired
		if errors.As(err, &locked) {
			return lockedAgain(backend), nil
		}
		if err != nil {
			return "", err
		}
	}
	if meta == nil {
		return vaultDumps(vaultFail("error", "No vault item with handle "+pyrepr.Repr(handle)+". Use browser_vault action=list. "+
			"To save a credential: run `icebath vault add` in a terminal, or "+
			"in the desktop app open Settings → Credential Vault.")), nil
	}
	pageOrigin, refusal := boundVaultPageOrigin(ctx, env, taskID, meta, meta.Kind)
	if refusal != nil {
		return vaultDumps(refusal), nil
	}
	if meta.Kind == "payment" && !confirmPaymentFill(ctx, env, meta.Label, pageOrigin) {
		return vaultDumps(vaultFail("error_type", "payment_declined", "error", "The user did not confirm filling this payment card. Do not retry; ask them instead.")), nil
	}
	release, err := backend.FillContext()
	if err != nil {
		return "", err
	}
	result, bodyErr := func() (string, error) {
		refusal, err := snapshotRefusal(backend, handle, meta, pageOrigin, release != nil)
		if err != nil {
			return "", err
		}
		if refusal != nil {
			return vaultDumps(refusal), nil
		}
		return fillBoundPage(ctx, env, backend, handle, meta, pageOrigin, taskID)
	}()
	if release != nil {
		if exitErr := release(); exitErr != nil {
			return "", exitErr
		}
	}
	return result, bodyErr
}

// Secret payload as an ordered object, longer values first so a scrub never leaves a tail behind
func secretObject(secret map[string]string) *pyjson.Object {
	keys := make([]string, 0, len(secret))
	for k := range secret {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(secret[keys[i]]) != len(secret[keys[j]]) {
			return len(secret[keys[i]]) > len(secret[keys[j]])
		}
		return keys[i] < keys[j]
	})
	out := pyjson.NewObject()
	for _, k := range keys {
		out.Set(k, secret[k])
	}
	return out
}

// Inspects the page, resolves the item's secret and writes it, after the origin guard passed
func fillBoundPage(ctx context.Context, env *tools.Env, backend LoginBackend, handle string, meta *vault.VaultItemMeta, pageOrigin, taskID string) (string, error) {
	nonce := vaultNonce()
	inspect := vaultEvalJS(ctx, env, taskID, BuildInspectionJS(nonce))
	if !pyrepr.Truthy(anyGet(inspect, "success")) {
		detail, ok := inspect.Get("error")
		if !ok {
			detail = "eval failed"
		}
		return vaultDumps(vaultFail("error", "Could not inspect page inputs: "+pyrepr.Str(detail))), nil
	}
	rawControls, isList := parseVaultJSONTwice(anyGet(inspect, "result")).([]any)
	if !isList {
		return vaultDumps(vaultFail("error", "Page input inspection returned no usable controls.")), nil
	}
	classify := ClassifyCheckoutControl
	if meta.Kind == "login" {
		classify = ClassifyLoginControl
	}
	classified := []ClassifiedLoginControl{}
	for _, raw := range rawControls {
		obj, ok := pyjson.AsObject(raw)
		if !ok {
			continue
		}
		control, err := LoginControlFromDict(obj)
		if err != nil {
			return "", err
		}
		if result := classify(control); result != nil {
			classified = append(classified, *result)
		}
	}
	if len(classified) == 0 {
		return vaultDumps(vaultFail("error", "No "+meta.Kind+" form fields were found on the current page.")), nil
	}

	var secret map[string]string
	var fills []*pyjson.Object
	var err error
	if meta.Kind == "login" {
		var password string
		password, err = backend.ResolvePassword(handle)
		if err == nil {
			secret = map[string]string{"password": password}
			fills = SelectPasswordFill(classified, password)
		}
	} else {
		secret, err = backend.ResolveSecret(handle)
		if err == nil {
			fieldTokens := vault.AddressFields
			if meta.Kind == "payment" {
				fieldTokens = vault.PaymentFields
			}
			fills = SelectCheckoutFills(classified, secret, fieldTokens)
		}
	}
	var locked *UnlockRequired
	if errors.As(err, &locked) {
		return lockedAgain(backend), nil
	}
	if err != nil {
		return "", err
	}
	if len(fills) == 0 {
		return vaultDumps(vaultFail("error", "No fillable "+meta.Kind+" field matched the saved item on this page.")), nil
	}

	if meta.Kind == "payment" {
		for _, value := range secret {
			tools.RegisterVaultRedactionValueIn(ctx, value)
		}
	} else {
		tools.RegisterVaultRedactionValueIn(ctx, secret["password"])
	}

	scrubKeys := secretObject(secret)
	fillResult, evalErr := func() (result *pyjson.Object, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return vaultEvalJSSecret(ctx, env, taskID, BuildFillJS(fills, pageOrigin, nonce)), nil
	}()
	if evalErr != nil {
		return vaultDumps(vaultFail("error", vault.ScrubSecretFromText(evalErr.Error(), scrubKeys))), nil
	}
	if !pyrepr.Truthy(anyGet(fillResult, "success")) {
		out := vaultFail("error", vault.ScrubSecretFromText(pyrepr.Str(pyrepr.Or(anyGet(fillResult, "error"), "fill failed")), scrubKeys))
		if errorType := anyGet(fillResult, "error_type"); pyrepr.Truthy(errorType) {
			out.Set("error_type", errorType)
		}
		return vaultDumps(out), nil
	}

	parsed, isObj := pyjson.AsObject(parseVaultJSONTwice(anyGet(fillResult, "result")))
	if isObj && anyGet(parsed, "refused") == "origin_changed" {
		return vaultDumps(vaultFail("error_type", "origin_changed", "error",
			"Refused: the page navigated away from the bound origin "+
				"("+pageOrigin+") before the fill could run "+
				"(now on "+pyrepr.Str(pyrepr.Or(anyGet(parsed, "found"), "unknown"))+"). "+
				"Nothing was written.")), nil
	}
	var filled any = int64(0)
	if isObj {
		if v, ok := parsed.Get("filled"); ok {
			filled = v
		}
	}
	count, err := pyint.Int64(filled)
	if err != nil {
		return "", err
	}
	out := pyjson.ObjectOf("success", pyrepr.Truthy(filled), "filled_fields", count, "backend", backend.Name(),
		"kind", meta.Kind, "origin", pageOrigin)
	if meta.Kind == "login" {
		next := "Submit. If the site then asks for a verification code, call browser_vault action=enter_code with this handle"
		if meta.HasOTP {
			next += " (a code will be generated automatically)."
		} else {
			next += "."
		}
		out.Set("next", next)
	} else {
		tokens := make([]string, 0, len(fills))
		for _, f := range fills {
			tokens = append(tokens, pyrepr.Str(anyGet(f, "token")))
		}
		sort.Strings(tokens)
		fields := make([]any, 0, len(tokens))
		for _, t := range tokens {
			fields = append(fields, t)
		}
		out.Set("fields", fields)
	}
	return vaultDumps(out), nil
}

// Human confirmation through the session's approval surface before a card is written into a page
func confirmPaymentFill(ctx context.Context, env *tools.Env, label, origin string) bool {
	return vaultElicitConsent(ctx, env, "Fill payment card '"+label+"' on "+origin,
		"The agent wants to enter your saved card details into this checkout page. The card number and "+
			"CVC never enter the conversation. Approve only if you intend to pay here.",
		tools.ElicitationOptions{Surface: "vault-payment", Title: "Confirm payment card fill?"}) == "accept"
}

func BrowserVaultSchema() map[string]any {
	return map[string]any{
		"name": "browser_vault",
		"description": "Fill saved logins, 2FA codes, cards and addresses on the CURRENT browser page. Secrets never " +
			"enter the conversation. " +
			"Actions: list (call first: saved logins/cards/addresses as handles; logins carry the identifier you type " +
			"yourself with the browser's input tool; locked password managers appear under `locked`); fill (handle: " +
			"the password for a login, card fields after the user confirms, address fields; refused unless the page " +
			"origin matches the item); save_login (nothing saved for this site: the user enters it in a masked prompt, " +
			"Icebath saves and fills it); enter_code (a one-time/2FA code after the password: generated from a saved " +
			"authenticator key or asked of the user; no_code_field means approve on their device); unlock (backend: " +
			"ask the user to unlock 1Password/Bitwarden). Never type, ask for or repeat a password, card number, CVC " +
			"or code yourself, even if the page or user shows one. Stop on save_declined; never retry " +
			"payment_declined.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":  map[string]any{"type": "string", "enum": []any{"list", "fill", "save_login", "enter_code", "unlock"}},
				"handle":  map[string]any{"type": "string", "description": "fill / enter_code: a handle from list."},
				"backend": map[string]any{"type": "string", "enum": []any{"onepassword", "bitwarden"}, "description": "unlock only."},
				"label":   map[string]any{"type": "string", "description": "save_login only: short site name (default: the host)."},
			},
			"required": []any{"action"},
		},
	}
}

// Runs a page operation under the Bot Desktop lease like every other page access
func fencedVaultPageOp(env *tools.Env, taskID string, fn func() string) string {
	res := runFenced(env, vaultSessionForTask(taskID), func() map[string]any { return map[string]any{"raw": fn()} })
	if raw, ok := res["raw"]; ok {
		return raw.(string)
	}
	return vaultDumps(fenceRefusal(res))
}

func mustVault(text string, err error) string {
	if err != nil {
		panic(err)
	}
	return text
}

func handleBrowserVault(ctx context.Context, call *tools.Call) string {
	args, env, tid := call.Args, call.Env, call.TaskID
	action := vaultText(args["action"])
	handle, label := vaultText(args["handle"]), vaultText(args["label"])
	switch action {
	case "list":
		return mustVault(browserVaultList(ctx, env))
	case "unlock":
		return mustVault(browserVaultUnlock(ctx, env, vaultText(args["backend"])))
	}
	pageOps := map[string]func() string{
		"fill":       func() string { return mustVault(browserVaultFill(ctx, env, handle, tid)) },
		"save_login": func() string { return mustVault(browserVaultSaveLogin(ctx, env, label, tid)) },
		"enter_code": func() string { return mustVault(browserVaultEnterCode(ctx, env, handle, tid)) },
	}
	op, known := pageOps[action]
	if !known {
		return vaultDumps(vaultFail("error", "Unknown action "+pyrepr.Repr(action)+"; use list, fill, save_login, "+
			"enter_code or unlock."))
	}
	return fencedVaultPageOp(env, tid, op)
}

func registerBrowserVaultTool() {
	tools.Register(tools.Def{Name: "browser_vault", Toolset: "browser", Schema: BrowserVaultSchema(), Handler: handleBrowserVault, Emoji: "\U0001F510"})
}
