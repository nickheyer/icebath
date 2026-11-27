package tools

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/tools/redact"
	"github.com/nickheyer/icebath/internal/vault"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Key a page holder's secret script answers with when it threw, so nothing of the page reaches the result
const evalFailed = "icebath_vault_eval_failed"

func vaultFail(pairs ...any) map[string]any {
	out := map[string]any{"success": false}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i].(string)] = pairs[i+1]
	}
	return out
}

// Result of the vault tool with its keys in the order they mean most
func vaultJSON(result map[string]any) string {
	return orderedJSON(result, "success", "error_type", "error", "handle", "backend", "already_unlocked", "kind", "items", "locked", "errors",
		"hint", "filled_fields", "origin", "identifier", "identifier_type", "fill", "source", "fields", "next")
}

// One vault call: the page it works on, through the page holder of the task or the supervisor of its session
type vaultCall struct {
	r    *registrant
	ctx  context.Context
	call *tools.Call
	cfg  *config.Config
	task string
}

func (v *vaultCall) holder() *pageHolder {
	if p := v.r.browser.Holder(v.cfg, v.task); p != nil {
		return &pageHolder{name: p.Title, evaluate: p.Evaluate}
	}
	return nil
}

type pageHolder struct {
	name     string
	evaluate func(ctx context.Context, cfg *config.Config, task, script string, secret bool) (any, error)
}

// Evaluates on a page holder, a failure of a secret script answered with fixed text only
func (v *vaultCall) holderEval(p *pageHolder, expression string, secret bool) map[string]any {
	if secret {
		expression = "(() => { try { return (" + expression + "); } catch (_) { return " + jsonText(map[string]any{evalFailed: true}) + "; } })()"
	}
	result, err := func() (result any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return p.evaluate(v.ctx, v.cfg, v.task, expression, secret)
	}()
	if err != nil {
		detail := ""
		if !secret {
			detail = ": " + strs.Head(err.Error(), 200)
		}
		return vaultFail("error_type", "eval_failed", "error", "The "+p.name+" browser could not run the vault script"+detail)
	}
	if secret {
		if parsed, ok := decodeJSON(result).(map[string]any); ok && len(parsed) == 1 && parsed[evalFailed] == true {
			return vaultFail("error_type", "eval_failed", "error", "The "+p.name+" browser could not run the vault script")
		}
	}
	return map[string]any{"success": true, "result": result}
}

// The task's supervisor, started on its session's endpoint when none runs; nil when no endpoint is known
func (v *vaultCall) supervisor() *supervisor {
	if s := v.r.supervisors.get(v.task); s != nil {
		return s
	}
	s := v.r.sessionFor(v.task)
	if s == nil || s.cdp == "" {
		return nil
	}
	sup, err := v.r.supervisors.ensure(v.cfg, v.task, resolveCDP(v.ctx, s.cdp))
	if err != nil {
		return nil
	}
	return sup
}

// Evaluates an inspection script on the page holder of the task, else over its supervisor
func (v *vaultCall) eval(expression string) map[string]any {
	if p := v.holder(); p != nil {
		return v.holderEval(p, expression, false)
	}
	s := v.supervisor()
	if s == nil {
		return vaultFail("error", "no supervised browser session for this task; open the page with browser_exec first")
	}
	got := s.evaluate(expression, true, true, 10*time.Second)
	if got["ok"] == true {
		return map[string]any{"success": true, "result": got["result"]}
	}
	return vaultFail("error", cmp.Or(text(got["error"]), "evaluate failed"))
}

// Evaluates a script that carries a secret over the supervisor's connection or the page holder, never on a command line
func (v *vaultCall) evalSecret(expression string) map[string]any {
	p := v.holder()
	var s *supervisor
	if p == nil {
		s = v.supervisor()
	}
	if p == nil && s == nil {
		return vaultFail("error_type", "supervisor_required", "error", "Vault fill requires the supervised browser session (direct CDP WebSocket), "+
			"and this task has none. Open the page with browser_exec and retry.")
	}
	if v.r.sharesScreen(v.ctx, v.cfg, v.r.sessionFor(v.task)) {
		if _, err := v.r.desktop.Admit(v.cfg); err != nil {
			return vaultFail("error_type", "human_has_control", "error", err.Error())
		}
	}
	if p != nil {
		return v.holderEval(p, expression, true)
	}
	got := s.evaluate(expression, true, true, 10*time.Second)
	if got["ok"] == true {
		return map[string]any{"success": true, "result": got["result"]}
	}
	kind := "eval_failed"
	if strings.Contains(strings.ToLower(text(got["error"])), "supervisor") {
		kind = "supervisor_required"
	}
	return vaultFail("error_type", kind, "error", cmp.Or(text(got["error"]), "eval failed"))
}

// JSON text decoded, anything else as it is
func decodeJSON(raw any) any {
	s, ok := raw.(string)
	if !ok {
		return raw
	}
	var parsed any
	if json.Unmarshal([]byte(s), &parsed) != nil {
		return raw
	}
	return parsed
}

func decodeTwice(raw any) any {
	parsed := decodeJSON(raw)
	if _, ok := parsed.(string); ok {
		parsed = decodeJSON(parsed)
	}
	return parsed
}

// Origin of the current page, empty for a blank one or one that can't be read
func (v *vaultCall) pageOrigin() string {
	res := v.eval("window.location.href")
	if res["success"] != true {
		return ""
	}
	href := strings.Trim(strings.Trim(strings.TrimSpace(text(res["result"])), `"`), "'")
	if href == "" || href == "about:blank" {
		return ""
	}
	origin, err := vault.NormalizeOrigin(href)
	if err != nil {
		return ""
	}
	return origin
}

// Script true when the selector matches in the document or any open shadow root
func deepProbe(selector string) string {
	return "((s) => { const w = (r) => r.querySelector(s) || Array.from(r.querySelectorAll('*')).some((e) => e.shadowRoot && w(e.shadowRoot)); " +
		"return !!w(document); })(" + jsonText(selector) + ")"
}

var tabProbes = map[string]string{
	"login":   deepProbe("input[type=password]"),
	"payment": deepProbe("input[autocomplete^=cc-], [name*=card i], [placeholder*=card i], [name*=cvc i], [name*=cvv i]"),
	"address": deepProbe("input[autocomplete^=address-], [autocomplete=postal-code], [name*=address i], [name*=zip i], [name*=postal i]"),
	"otp": deepProbe("input[autocomplete=one-time-code], input[name*=otp i], input[name*=code i], input[id*=otp i], input[id*=code i], " +
		"input[name*=totp i], input[aria-label*=code i]"),
}

// Points the supervisor at the open tab of the origin that holds a form of the kind, empty when none was found
func (v *vaultCall) focus(origin, kind string) string {
	if v.holder() != nil {
		return ""
	}
	s := v.supervisor()
	if s == nil {
		return ""
	}
	focused := s.focus(origin, tabProbes[kind], 10*time.Second)
	if focused["ok"] != true {
		return ""
	}
	return cmp.Or(origin, text(focused["url"]))
}

// The exact origins an item may be filled on
func origins(item *vault.Item) []string {
	if len(item.Origins) > 0 {
		return slices.Clone(item.Origins)
	}
	if item.Origin != "" {
		return []string{item.Origin}
	}
	return nil
}

// Origin of the page when it is one the item was saved for, else the refusal
func (v *vaultCall) bound(item *vault.Item, kind string) (string, map[string]any) {
	allowed := origins(item)
	if len(allowed) == 0 {
		return "", vaultFail("error_type", "no_origin", "error", fmt.Sprintf("Vault item %s has no bound origin; %s items are filled only on the site "+
			"they were saved for.", pyQuote(item.ID), item.Kind))
	}
	page := ""
	for _, candidate := range allowed {
		if o := v.focus(candidate, kind); o != "" {
			page = o
			break
		}
	}
	page = cmp.Or(page, v.pageOrigin())
	if page == "" {
		return "", vaultFail("error", "Could not determine the current page origin. Navigate to the login page first.")
	}
	if !slices.Contains(allowed, page) {
		return "", vaultFail("error_type", "origin_mismatch", "error", "Refused: current page origin ("+page+") does not match the vault item's bound "+
			"origin(s) ("+strings.Join(allowed, ", ")+"). Vault fills only run on the exact origin(s) the credential was saved for.")
	}
	return page, nil
}

// Whether the user can be asked here
func (v *vaultCall) canAsk() bool { return v.call.Env.Clarify != nil }

// Asks the user the questions, the answers by id; false when they were not all submitted
func (v *vaultCall) ask(questions ...tools.Question) (map[string]string, bool) {
	reply, err := v.call.Env.Clarify(v.ctx, questions)
	if err != nil || reply.Outcome != tools.Submitted {
		return nil, false
	}
	out := map[string]string{}
	for _, q := range questions {
		if values := reply.Answers[q.ID]; len(values) > 0 {
			out[q.ID] = values[0]
		}
	}
	return out, true
}

// Handles and metadata of every source the profile reads, a locked manager listed under locked
func (v *vaultCall) list() string {
	items, locked, failures := []any{}, []any{}, []any{}
	for _, login := range v.r.vault.Logins(v.cfg) {
		if login.NeedsUnlock() && !login.Unlocked() {
			unlock := "unavailable_in_this_session"
			if v.canAsk() {
				unlock = "browser_vault action=unlock"
			}
			locked = append(locked, orderedMap{{"backend", login.Name}, {"display_name", login.Title}, {"unlock", unlock}})
			continue
		}
		got, err := login.Items(v.ctx)
		if err != nil {
			failures = append(failures, orderedMap{{"backend", login.Name}, {"error", strs.Head(err.Error(), 200)}})
			continue
		}
		for _, item := range got {
			entry := orderedMap{{"handle", item.ID}, {"backend", login.Name}, {"label", item.Label}, {"kind", item.Kind}, {"origin", strs.Nullable(item.Origin)},
				{"available", item.Kind == "login" || item.Origin != ""}}
			if len(item.Origins) > 1 {
				entry = append(entry, pair{"allowed_origins", item.Origins})
			}
			if item.OTP {
				entry = append(entry, pair{"two_factor", "automatic"})
			} else if login.NeedsUnlock() {
				entry = append(entry, pair{"two_factor", "automatic if the manager stores a TOTP seed, else the user is asked"})
			}
			if item.Identifier != "" {
				entry = append(entry, pair{"identifier", item.Identifier}, pair{"identifier_type", item.IdentifierType})
			}
			items = append(items, entry)
		}
	}
	out := map[string]any{"success": true, "items": items}
	if len(items) == 0 {
		out["hint"] = "No saved logins. On a login page, call browser_vault action=save_login to ask the user to save one. Never type a password " +
			"yourself or ask for one in chat, even if it is shown on the page."
	}
	if len(locked) > 0 {
		out["locked"] = locked
	}
	if len(failures) > 0 {
		out["errors"] = failures
	}
	return vaultJSON(out)
}

// Asks the user in a masked prompt to unlock a password manager for this session
func (v *vaultCall) unlock(name string) string {
	var login *vault.Login
	for _, l := range v.r.vault.Logins(v.cfg) {
		if l.Name == name && l.NeedsUnlock() {
			login = &l
			break
		}
	}
	if login == nil {
		return vaultJSON(vaultFail("error", "No unlockable vault backend named "+pyQuote(name)+"."))
	}
	if login.Unlocked() {
		return vaultJSON(map[string]any{"success": true, "backend": login.Name, "already_unlocked": true})
	}
	if !v.canAsk() {
		return vaultJSON(vaultFail("error_type", "unlock_unavailable", "error", login.Title+" is locked and this session cannot prompt for the master "+
			"password (headless/cron/API). Unlock it from an interactive Icebath session first."))
	}
	answers, ok := v.ask(tools.Question{ID: "master", Text: "Unlock " + login.Title + ": master password (used for this session only, never sent to the " +
		"model)", Secret: true})
	master := answers["master"]
	if !ok || master == "" {
		return vaultJSON(vaultFail("error_type", "unlock_cancelled", "error", "The user declined to unlock "+login.Title+"."))
	}
	err := login.Unlock(v.ctx, master, v.call.SessionID)
	clear(answers)
	if err != nil {
		return vaultJSON(vaultFail("error_type", "unlock_failed", "error", strs.Head(err.Error(), 300)))
	}
	return vaultJSON(map[string]any{"success": true, "backend": login.Name})
}

func host(origin string) string {
	if _, h, found := strings.Cut(origin, "://"); found {
		return h
	}
	return origin
}

// Asks the user for the login of the current page, saves it for that origin and fills its password
func (v *vaultCall) saveLogin(label string) string {
	v.focus("", "login")
	origin := v.pageOrigin()
	if origin == "" {
		return vaultJSON(vaultFail("error", "Open the site's login page first; the login is saved for that page's origin."))
	}
	if !v.canAsk() {
		return vaultJSON(vaultFail("error_type", "prompt_unavailable", "error", "This session cannot ask the user for a login (headless/cron/API). "+
			"Tell them to run `icebath vault add` for "+origin+"."))
	}
	site := cmp.Or(strings.TrimSpace(label), host(origin))
	answers, ok := v.ask(
		tools.Question{ID: "identifier", Text: "Save a login for " + host(origin) + " — email, phone or username (leave empty to not save)"},
		tools.Question{ID: "password", Text: "Password for " + host(origin) + " (saved in the vault for " + origin + ", never sent to the model)",
			Secret: true})
	if !ok || answers["password"] == "" || answers["identifier"] == "" {
		clear(answers)
		return vaultJSON(vaultFail("error_type", "save_declined", "error", "The user chose not to save a login for this site. Do not ask again this turn."))
	}
	identifier := strings.TrimSpace(answers["identifier"])
	kind := "username"
	if strings.Contains(identifier, "@") {
		kind = "email"
	} else if digits := strings.TrimLeft(identifier, "+"); digits != "" && strings.Trim(digits, "0123456789") == "" {
		kind = "phone"
	}
	item, err := v.r.vault.Store(v.cfg).Add("login", site, origin, map[string]string{"identifier_type": kind, "identifier": identifier,
		"password": answers["password"]})
	clear(answers)
	if err != nil {
		return vaultJSON(vaultFail("error_type", "save_failed", "error", strs.Head(err.Error(), 200)))
	}
	var filled any
	json.Unmarshal([]byte(v.fill(item.ID)), &filled)
	return orderedJSON(map[string]any{"success": true, "handle": item.ID, "origin": origin, "identifier": identifier, "identifier_type": kind,
		"fill": filled, "next": "Type the identifier into the username field if the form has one, then submit."}, "success", "handle", "origin",
		"identifier", "identifier_type", "fill", "next")
}

func nonce() string {
	raw := make([]byte, 8)
	rand.Read(raw)
	return hex.EncodeToString(raw)
}

// Controls of the inspection that are objects
func controls(raw any) ([]control, error) {
	if !config.Truthy(raw) {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		switch raw.(type) {
		case map[string]any, string:
			return nil, nil
		}
		return nil, fmt.Errorf("'%T' object is not iterable", raw)
	}
	var out []control
	for _, entry := range list {
		if obj, ok := entry.(map[string]any); ok {
			c, err := controlOf(obj)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
	}
	return out, nil
}

// The source and item of a handle, no item when nothing is saved under it or its manager is locked
func (v *vaultCall) saved(handle string) (vault.Login, *vault.Item, error) {
	if handle == "" {
		return vault.Login{}, nil, nil
	}
	login, ok := v.r.vault.Login(v.cfg, handle)
	if !ok {
		return vault.Login{}, nil, nil
	}
	if login.NeedsUnlock() && !login.Unlocked() {
		return login, nil, nil
	}
	item, err := login.Item(v.ctx, handle)
	var locked *vault.Locked
	if errors.As(err, &locked) {
		return login, nil, nil
	}
	return login, item, err
}

// Fills the one time code the page asks for: made from a saved login's authenticator key, else typed by the user
func (v *vaultCall) enterCode(handle string) string {
	login, item, err := v.saved(handle)
	if err != nil {
		return vaultJSON(vaultFail("error_type", "backend_unavailable", "error", strs.Head(err.Error(), 200)))
	}
	origin := ""
	if item != nil {
		bound, refusal := v.bound(item, "otp")
		if refusal != nil {
			if refusal["error_type"] == "origin_mismatch" {
				refusal["next"] = "If this page asks for the code of a different account, call enter_code without a handle so the user is asked."
			}
			return vaultJSON(refusal)
		}
		origin = bound
	} else {
		v.focus("", "otp")
		if origin = v.pageOrigin(); origin == "" {
			return vaultJSON(vaultFail("error", "No page with a code field is open."))
		}
	}
	site := host(origin)
	n := nonce()
	inspect := v.eval(inspectionJS(n))
	var raw any
	if inspect["success"] == true {
		raw = decodeTwice(inspect["result"])
	}
	found, err := controls(raw)
	if err != nil {
		return tools.ErrorResult(err)
	}
	codes := classifyCodes(found)
	if len(codes) == 0 {
		return vaultJSON(vaultFail("error_type", "no_code_field", "error", "No one-time-code field on the current page. If the site wants a passkey, "+
			"hardware key or an approval tap in an app, tell the user to complete it on their device and wait for the page to move on."))
	}
	write := func(code, source string) string {
		redact.Fill(v.cfg.Profile(), code)
		result := v.evalSecret(fillJS(codeFills(codes, code), origin, n))
		if result["success"] != true {
			return vaultJSON(vaultFail("error", strs.Head(cmp.Or(text(result["error"]), "fill failed"), 200)))
		}
		parsed, _ := decodeTwice(result["result"]).(map[string]any)
		if parsed["refused"] == "origin_changed" {
			return vaultJSON(vaultFail("error_type", "origin_changed", "error", "The page navigated before the code could be entered. Nothing was written."))
		}
		count := cast.ToInt(parsed["filled"])
		return vaultJSON(map[string]any{"success": count != 0, "filled_fields": count, "origin": origin, "source": source,
			"next": "Submit the form (many sites auto-submit when the last digit lands)."})
	}
	if item != nil {
		if code, ok, err := login.OTP(v.ctx, handle); err == nil && ok && code != "" {
			return write(code, login.Name)
		}
	}
	if !v.canAsk() {
		return vaultJSON(vaultFail("error_type", "prompt_unavailable", "error", site+" asks for a one-time code and this session cannot ask the user "+
			"(headless/cron/API). Save an authenticator key for this login so codes can be generated automatically."))
	}
	answers, ok := v.ask(tools.Question{ID: "code", Text: site + " asks for a one-time code: enter it (never sent to the model)", Secret: true})
	code := strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(answers["code"]), " ", ""), "-", "")
	clear(answers)
	if !ok || code == "" {
		return vaultJSON(vaultFail("error_type", "code_declined", "error", "The user did not enter a code. Do not ask again this turn."))
	}
	return write(code, "user")
}

func lockedAgain(login vault.Login) string {
	return vaultJSON(vaultFail("error_type", "unlock_required", "error", login.Title+" locked again; call browser_vault action=unlock."))
}

// Fills the current page from a handle, the result saying only how many fields and what of
func (v *vaultCall) fill(handle string) string {
	login, ok := v.r.vault.Login(v.cfg, handle)
	if ok && login.NeedsUnlock() && !login.Unlocked() {
		var unlocked map[string]any
		json.Unmarshal([]byte(v.unlock(login.Name)), &unlocked)
		if unlocked["success"] != true {
			return vaultJSON(unlocked)
		}
	}
	var item *vault.Item
	if ok {
		var err error
		item, err = login.Item(v.ctx, handle)
		var locked *vault.Locked
		if errors.As(err, &locked) {
			return lockedAgain(login)
		}
		if err != nil {
			return vaultJSON(vaultFail("error_type", "backend_unavailable", "error", strs.Head(err.Error(), 200)))
		}
	}
	if item == nil {
		return vaultJSON(vaultFail("error", "No vault item with handle "+pyQuote(handle)+". Use browser_vault action=list. To save a credential: run "+
			"`icebath vault add` in a terminal."))
	}
	page, refusal := v.bound(item, item.Kind)
	if refusal != nil {
		return vaultJSON(refusal)
	}
	if item.Kind == "payment" && !v.confirmPayment(item.Label, page) {
		return vaultJSON(vaultFail("error_type", "payment_declined", "error", "The user did not confirm filling this payment card. Do not retry; ask "+
			"them instead."))
	}
	return v.fillBound(login, handle, item, page)
}

// Inspects the page, reads the item's secret and writes it, once the origin passed
func (v *vaultCall) fillBound(login vault.Login, handle string, item *vault.Item, page string) string {
	n := nonce()
	inspect := v.eval(inspectionJS(n))
	if inspect["success"] != true {
		return vaultJSON(vaultFail("error", "Could not inspect page inputs: "+cmp.Or(text(inspect["error"]), "eval failed")))
	}
	raw, isList := decodeTwice(inspect["result"]).([]any)
	if !isList {
		return vaultJSON(vaultFail("error", "Page input inspection returned no usable controls."))
	}
	classify := classifyCheckout
	if item.Kind == "login" {
		classify = classifyLogin
	}
	var matched []classified
	for _, entry := range raw {
		obj, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		c, err := controlOf(obj)
		if err != nil {
			return tools.ErrorResult(err)
		}
		if result := classify(c); result != nil {
			matched = append(matched, *result)
		}
	}
	if len(matched) == 0 {
		return vaultJSON(vaultFail("error", "No "+item.Kind+" form fields were found on the current page."))
	}
	var secret map[string]string
	var fills []fill
	var err error
	if item.Kind == "login" {
		var password string
		if password, err = login.Password(v.ctx, handle); err == nil {
			secret = map[string]string{"password": password}
			fills = passwordFill(matched, password)
		}
	} else if secret, err = login.Secret(v.ctx, handle); err == nil {
		fields := vault.AddressFields
		if item.Kind == "payment" {
			fields = vault.PaymentFields
		}
		fills = checkoutFills(matched, secret, fields)
	}
	var locked *vault.Locked
	if errors.As(err, &locked) {
		return lockedAgain(login)
	}
	if err != nil {
		return vaultJSON(vaultFail("error_type", "backend_unavailable", "error", strs.Head(vault.Scrub(err.Error(), secret), 200)))
	}
	defer clear(secret)
	if len(fills) == 0 {
		return vaultJSON(vaultFail("error", "No fillable "+item.Kind+" field matched the saved item on this page."))
	}
	if item.Kind == "payment" {
		for _, value := range secret {
			redact.Fill(v.cfg.Profile(), value)
		}
	} else {
		redact.Fill(v.cfg.Profile(), secret["password"])
	}
	result, evalErr := func() (result map[string]any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return v.evalSecret(fillJS(fills, page, n)), nil
	}()
	if evalErr != nil {
		return vaultJSON(vaultFail("error", vault.Scrub(evalErr.Error(), secret)))
	}
	if result["success"] != true {
		out := vaultFail("error", vault.Scrub(cmp.Or(text(result["error"]), "fill failed"), secret))
		if kind := result["error_type"]; config.Truthy(kind) {
			out["error_type"] = kind
		}
		return vaultJSON(out)
	}
	parsed, _ := decodeTwice(result["result"]).(map[string]any)
	if parsed["refused"] == "origin_changed" {
		return vaultJSON(vaultFail("error_type", "origin_changed", "error", "Refused: the page navigated away from the bound origin ("+page+") before the "+
			"fill could run (now on "+cmp.Or(text(parsed["found"]), "unknown")+"). Nothing was written."))
	}
	count := cast.ToInt(parsed["filled"])
	out := map[string]any{"success": count != 0, "filled_fields": count, "backend": login.Name, "kind": item.Kind, "origin": page}
	if item.Kind == "login" {
		next := "Submit. If the site then asks for a verification code, call browser_vault action=enter_code with this handle"
		if item.OTP {
			next += " (a code will be generated automatically)."
		} else {
			next += "."
		}
		out["next"] = next
	} else {
		tokens := make([]string, len(fills))
		for i, f := range fills {
			tokens[i] = f.Token
		}
		slices.Sort(tokens)
		out["fields"] = tokens
	}
	return vaultJSON(out)
}

// The user's yes, asked every time, before a card is written into a page
func (v *vaultCall) confirmPayment(label, origin string) bool {
	if v.call.Env.Gate == nil {
		return false
	}
	return v.call.Env.Gate(v.ctx, tools.Gate{Key: "vault-payment", Description: "Fill payment card '" + label + "' on " + origin,
		Target: "The agent wants to enter your saved card details into this checkout page. The card number and CVC never enter the conversation. " +
			"Approve only if you intend to pay here.", Strict: true, Noun: "payment card fill",
		Unanswered: "The user did not confirm filling this payment card."}) == nil
}

// Key and value in an ordered object
type pair struct {
	key   string
	value any
}

type orderedMap []pair

func (o orderedMap) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range o {
		if i > 0 {
			b.WriteString(", ")
		}
		key, _ := json.Marshal(p.key)
		b.Write(key)
		b.WriteString(": ")
		b.WriteString(tools.JSON(p.value))
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

const vaultAbout = "Fill saved logins, 2FA codes, cards and addresses on the CURRENT browser page. Secrets never enter the conversation. Actions: " +
	"list (call first: saved logins/cards/addresses as handles; logins carry the identifier you type yourself with the browser's input tool; " +
	"locked password managers appear under `locked`); fill (handle: the password for a login, card fields after the user confirms, address " +
	"fields; refused unless the page origin matches the item); save_login (nothing saved for this site: the user enters it in a masked prompt, " +
	"Icebath saves and fills it); enter_code (a one-time/2FA code after the password: generated from a saved authenticator key or asked of the " +
	"user; no_code_field means approve on their device); unlock (backend: ask the user to unlock 1Password/Bitwarden). Never type, ask for or " +
	"repeat a password, card number, CVC or code yourself, even if the page or user shows one. Stop on save_declined; never retry payment_declined."

// Schema of browser_vault, the backends it can unlock those the profile reads
func (r *registrant) vaultSchema(env *tools.Env) map[string]any {
	backend := map[string]any{"type": "string", "description": "unlock only."}
	if env != nil {
		var names []string
		for _, login := range r.vault.Logins(env.Config) {
			if login.NeedsUnlock() {
				names = append(names, login.Name)
			}
		}
		if len(names) > 0 {
			backend["enum"] = names
		}
	}
	return map[string]any{"description": vaultAbout, "parameters": map[string]any{"type": "object", "required": []string{"action"},
		"properties": map[string]any{
			"action":  map[string]any{"type": "string", "enum": []string{"list", "fill", "save_login", "enter_code", "unlock"}},
			"handle":  tools.Property("string", "fill / enter_code: a handle from list."),
			"backend": backend,
			"label":   tools.Property("string", "save_login only: short site name (default: the host)."),
		}}}
}

// Runs browser_vault, a page action under the Bot Desktop lease like every other page access
func (r *registrant) vaultTool(ctx context.Context, call *tools.Call) string {
	v := &vaultCall{r: r, ctx: ctx, call: call, cfg: call.Env.Config, task: cmp.Or(call.SessionID, "default")}
	action := text(call.Args["action"])
	handle, label := text(call.Args["handle"]), text(call.Args["label"])
	switch action {
	case "list":
		return v.list()
	case "unlock":
		return v.unlock(text(call.Args["backend"]))
	}
	ops := map[string]func() string{
		"fill":       func() string { return v.fill(handle) },
		"save_login": func() string { return v.saveLogin(label) },
		"enter_code": func() string { return v.enterCode(handle) },
	}
	op, known := ops[action]
	if !known {
		return vaultJSON(vaultFail("error", "Unknown action "+pyQuote(action)+"; use list, fill, save_login, enter_code or unlock."))
	}
	out, refusal, _ := r.fenced(ctx, v.cfg, r.sessionFor(v.task), func() (any, error) { return op(), nil })
	if refusal != nil {
		return refusalJSON(refusal)
	}
	return out.(string)
}
