package browser

import (
	"regexp"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var loginAutofillTokens = []string{"username", "email", "tel", "current-password"}

var paymentAutofillTokens = []string{"cc-number", "cc-name", "cc-exp", "cc-exp-month", "cc-exp-year", "cc-csc"}

var addressAutofillTokens = []string{"address-line1", "address-line2", "address-level2", "address-level1",
	"postal-code", "country-name", "country"}

type checkoutHeuristic struct {
	pattern *regexp.Regexp
	token   string
}

var checkoutHeuristics = []checkoutHeuristic{
	{regexp.MustCompile(`\b(?:card\s*number|cardnumber|ccnumber|cc\s*num|pan)\b`), "cc-number"},
	{regexp.MustCompile(`\b(?:name\s*on\s*card|cardholder|cc\s*name|ccname)\b`), "cc-name"},
	{regexp.MustCompile(`\b(?:cvc|cvv|csc|security\s*code|card\s*code)\b`), "cc-csc"},
	{regexp.MustCompile(`\b(?:exp(?:iry|iration)?\s*month|exp\s*mm|ccmonth)\b`), "cc-exp-month"},
	{regexp.MustCompile(`\b(?:exp(?:iry|iration)?\s*year|exp\s*yy(?:yy)?|ccyear)\b`), "cc-exp-year"},
	{regexp.MustCompile(`\b(?:exp(?:iry|iration)?(?:\s*date)?|mm\s*yy|valid\s*thru)\b`), "cc-exp"},
	{regexp.MustCompile(`\b(?:address\s*(?:line\s*)?2|apt|suite|unit)\b`), "address-line2"},
	{regexp.MustCompile(`\b(?:address(?:\s*line\s*1)?|street)\b`), "address-line1"},
	{regexp.MustCompile(`\b(?:city|town|locality)\b`), "address-level2"},
	{regexp.MustCompile(`\b(?:state|province|region|county)\b`), "address-level1"},
	{regexp.MustCompile(`\b(?:zip|postal|postcode)\b`), "postal-code"},
	{regexp.MustCompile(`\b(?:country)\b`), "country-name"},
}

var (
	reExcludedPassword = regexp.MustCompile(`\b(?:new|confirm|create|repeat)\s*password\b`)
	reLoginEmail       = regexp.MustCompile(`\b(?:e[\s-]?mail|email address)\b`)
	reLoginTel         = regexp.MustCompile(`\b(?:phone|telephone|mobile)\b`)
	reLoginUsername    = regexp.MustCompile(`\b(?:user\s*name|username|login|account|member|membership|mileageplus)\b`)
	reLoginOTP         = regexp.MustCompile(`\b(?:one[\s-]?time|verification|security|auth(?:entication|enticator)?|2fa|two[\s-]?factor|mfa|totp|otp|` +
		`passcode|sms)\b.*\b(?:code|pin|token)\b|\b(?:otp|totp|2fa|mfa|verification\s*code|passcode)\b`)
	reNonAlnumRun = regexp.MustCompile(`[^a-z0-9]+`)
)

// Compatibility decomposed lower case text with every other run collapsed to one space
func normalizeControlText(value string) string {
	value = strings.ToLower(norm.NFKD.String(value))
	return strings.TrimSpace(reNonAlnumRun.ReplaceAllString(value, " "))
}

// Descriptor of a visible input control as inspected in the page
type LoginControl struct {
	Autocomplete string
	FormIndex    *int
	Index        int
	Label        string
	Name         string
	Type         string
	MaxLength    *int
}

func optionalControlInt(raw *pyjson.Object, camel, snake string) (*int, error) {
	v, ok := raw.Get(camel)
	if !ok {
		v, _ = raw.Get(snake)
	}
	if v == nil {
		return nil, nil
	}
	n, err := pyint.Int64(v)
	if err != nil {
		return nil, err
	}
	out := int(n)
	return &out, nil
}

// Control built from the inspection script's record, either key spelling accepted
func LoginControlFromDict(raw *pyjson.Object) (LoginControl, error) {
	formIndex, err := optionalControlInt(raw, "formIndex", "form_index")
	if err != nil {
		return LoginControl{}, err
	}
	maxLength, err := optionalControlInt(raw, "maxLength", "max_length")
	if err != nil {
		return LoginControl{}, err
	}
	index, err := pyint.Int64(pyrepr.Or(anyGet(raw, "index"), int64(0)))
	if err != nil {
		return LoginControl{}, err
	}
	return LoginControl{Autocomplete: vaultText(anyGet(raw, "autocomplete")), FormIndex: formIndex, Index: int(index),
		Label: vaultText(anyGet(raw, "label")), Name: vaultText(anyGet(raw, "name")), Type: vaultText(anyGet(raw, "type")),
		MaxLength: maxLength}, nil
}

type ClassifiedLoginControl struct {
	Control LoginControl
	Score   int
	Token   string
}

func controlSearchable(c LoginControl) string {
	parts := []string{}
	for _, part := range []string{c.Name, c.Label} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return normalizeControlText(strings.Join(parts, " "))
}

func hasToken(tokens []string, token string) bool {
	for _, t := range tokens {
		if t == token {
			return true
		}
	}
	return false
}

// Classifies one control as a login fill target, nil when it is none
func ClassifyLoginControl(control LoginControl) *ClassifiedLoginControl {
	tokens := strings.Fields(strings.ToLower(control.Autocomplete))
	if hasToken(tokens, "new-password") || hasToken(tokens, "one-time-code") {
		return nil
	}
	for _, token := range loginAutofillTokens {
		if hasToken(tokens, token) {
			return &ClassifiedLoginControl{control, 100, token}
		}
	}
	searchable := controlSearchable(control)
	if reExcludedPassword.MatchString(searchable) {
		return nil
	}
	switch control.Type {
	case "password":
		return &ClassifiedLoginControl{control, 90, "current-password"}
	case "email":
		return &ClassifiedLoginControl{control, 85, "email"}
	case "tel":
		return &ClassifiedLoginControl{control, 85, "tel"}
	}
	if reLoginEmail.MatchString(searchable) {
		return &ClassifiedLoginControl{control, 75, "email"}
	}
	if reLoginTel.MatchString(searchable) {
		return &ClassifiedLoginControl{control, 75, "tel"}
	}
	if reLoginUsername.MatchString(searchable) {
		return &ClassifiedLoginControl{control, 70, "username"}
	}
	return nil
}

// The controls taking a second factor code in DOM order, autocomplete one-time-code being authoritative
func ClassifyOTPControls(controls []LoginControl) []ClassifiedLoginControl {
	out := []ClassifiedLoginControl{}
	for _, c := range controls {
		if hasToken(strings.Fields(strings.ToLower(c.Autocomplete)), "one-time-code") {
			out = append(out, ClassifiedLoginControl{c, 100, "one-time-code"})
			continue
		}
		switch c.Type {
		case "text", "tel", "number", "password", "":
		default:
			continue
		}
		if reLoginOTP.MatchString(controlSearchable(c)) {
			out = append(out, ClassifiedLoginControl{c, 70, "one-time-code"})
		}
	}
	return out
}

// Highest score first, DOM order breaking ties
func bestControl(candidates []ClassifiedLoginControl) ClassifiedLoginControl {
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.Score > best.Score || (c.Score == best.Score && c.Control.Index < best.Control.Index) {
			best = c
		}
	}
	return best
}

func controlsWithToken(classified []ClassifiedLoginControl, token string) []ClassifiedLoginControl {
	out := []ClassifiedLoginControl{}
	for _, c := range classified {
		if c.Token == token {
			out = append(out, c)
		}
	}
	return out
}

// One fill for the best current-password control, empty without a password field or password
func SelectPasswordFill(classified []ClassifiedLoginControl, password string) []*pyjson.Object {
	passwords := controlsWithToken(classified, "current-password")
	if len(passwords) == 0 || password == "" {
		return []*pyjson.Object{}
	}
	return []*pyjson.Object{pyjson.ObjectOf("index", int64(bestControl(passwords).Control.Index), "token", "current-password", "value", password)}
}

// Classifies one control as a payment or address fill target, password and email inputs never matching by text
func ClassifyCheckoutControl(control LoginControl) *ClassifiedLoginControl {
	tokens := strings.Fields(strings.ToLower(control.Autocomplete))
	for _, token := range append(append([]string{}, paymentAutofillTokens...), addressAutofillTokens...) {
		if hasToken(tokens, token) {
			if token == "country" {
				token = "country-name"
			}
			return &ClassifiedLoginControl{control, 100, token}
		}
	}
	if control.Type == "password" || control.Type == "email" {
		return nil
	}
	searchable := controlSearchable(control)
	for _, h := range checkoutHeuristics {
		if h.pattern.MatchString(searchable) {
			return &ClassifiedLoginControl{control, 70, h.token}
		}
	}
	return nil
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func zfill2(s string) string {
	sign := ""
	if strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		sign, s = s[:1], s[1:]
	}
	for len([]rune(sign+s)) < 2 {
		s = "0" + s
	}
	return sign + s
}

// Maps a payment or address payload onto the best control per token, a combined expiry replacing month and year
func SelectCheckoutFills(classified []ClassifiedLoginControl, secret map[string]string, fieldTokens *pyjson.Object) []*pyjson.Object {
	order := []string{}
	values := map[string]string{}
	set := func(token, value string) {
		if _, seen := values[token]; !seen {
			order = append(order, token)
		}
		values[token] = value
	}
	for _, field := range fieldTokens.Keys {
		if secret[field] != "" {
			set(pyrepr.Str(fieldTokens.Vals[field]), secret[field])
		}
	}
	month, hasMonth := values["cc-exp-month"]
	year, hasYear := values["cc-exp-year"]
	if hasMonth && hasYear {
		set("cc-exp", zfill2(month)+"/"+lastRunes(year, 2))
	}
	fills := []*pyjson.Object{}
	combined := false
	for _, token := range order {
		candidates := controlsWithToken(classified, token)
		if len(candidates) > 0 {
			fills = append(fills, pyjson.ObjectOf("index", int64(bestControl(candidates).Control.Index), "token", token, "value", values[token]))
			combined = combined || token == "cc-exp"
		}
	}
	if !combined {
		return fills
	}
	kept := []*pyjson.Object{}
	for _, f := range fills {
		if token := anyGet(f, "token"); token != "cc-exp-month" && token != "cc-exp-year" {
			kept = append(kept, f)
		}
	}
	return kept
}

// Attribute each inspection stamps on every input as nonce and index
const InspectionStampAttr = "data-icebath-vault-slot"

// One fill per adjacent single character box of one form, else the best field takes the whole code
func BuildOTPFills(otpControls []ClassifiedLoginControl, code string) []*pyjson.Object {
	best := otpControls[0]
	for _, c := range otpControls[1:] {
		if c.Score > best.Score {
			best = c
		}
	}
	boxes := []ClassifiedLoginControl{}
	for _, c := range otpControls {
		if c.Control.MaxLength != nil && *c.Control.MaxLength == 1 {
			boxes = append(boxes, c)
		}
	}
	sort.SliceStable(boxes, func(i, j int) bool { return boxes[i].Control.Index < boxes[j].Control.Index })
	digits := []rune(code)
	if len(boxes) == len(digits) && otpBoxesAdjacent(boxes) {
		out := make([]*pyjson.Object, 0, len(boxes))
		for i, b := range boxes {
			out = append(out, pyjson.ObjectOf("index", int64(b.Control.Index), "token", "one-time-code", "value", string(digits[i])))
		}
		return out
	}
	return []*pyjson.Object{pyjson.ObjectOf("index", int64(best.Control.Index), "token", "one-time-code", "value", code)}
}

// Whether the boxes share exactly one form and sit next to each other in DOM order
func otpBoxesAdjacent(boxes []ClassifiedLoginControl) bool {
	type formKey struct {
		grouped bool
		index   int
	}
	forms := map[formKey]bool{}
	for _, b := range boxes {
		key := formKey{}
		if b.Control.FormIndex != nil {
			key = formKey{true, *b.Control.FormIndex}
		}
		forms[key] = true
	}
	if len(forms) != 1 {
		return false
	}
	for i := 1; i < len(boxes); i++ {
		if boxes[i].Control.Index-boxes[i-1].Control.Index != 1 {
			return false
		}
	}
	return true
}

func jsonText(v any) string {
	text, err := pyjson.Dumps(v)
	if err != nil {
		panic(err)
	}
	return text
}

// Page script that stamps every input with the nonce and reports the visible controls
func BuildInspectionJS(nonce string) string {
	return strings.ReplaceAll(loginControlInspectionJSTemplate, "__NONCE__", jsonText(nonce))
}

const loginControlInspectionJSTemplate = `(() => {
  const nonce = __NONCE__;
  const elements = [];
  const forms = [];
  (function walk(root) {
    for (const e of root.querySelectorAll("*")) {
      if (e.tagName === "INPUT" || e.tagName === "SELECT") elements.push(e);
      else if (e.tagName === "FORM") forms.push(e);
      if (e.shadowRoot) walk(e.shadowRoot);
    }
  })(document);
  elements.forEach((element, index) => element.setAttribute("data-icebath-vault-slot", nonce + ":" + index));
  const out = elements.flatMap((element, index) => {
    if (element.disabled || element.readOnly) return [];
    if (["hidden", "submit", "button", "reset", "file", "image", "checkbox", "radio"].includes(element.type)) return [];
    const style = getComputedStyle(element);
    if (style.display === "none" || style.visibility === "hidden" || element.getClientRects().length === 0) return [];
    const labels = element.labels ? Array.from(element.labels, (l) => l.textContent || "") : [];
    const ariaText = (element.getAttribute("aria-labelledby") || "")
      .split(/\s+/).filter(Boolean)
      .map((id) => { const r = element.getRootNode(); const n = (r.getElementById ? r.getElementById(id) : null) || document.getElementById(id); return n ? (n.textContent || "") : ""; })
      .join(" ");
    const resolvedFormIndex = element.form ? forms.indexOf(element.form) : -1;
    return [{
      autocomplete: element.autocomplete || "",
      formIndex: resolvedFormIndex >= 0 ? resolvedFormIndex : null,
      index,
      maxLength: element.maxLength > 0 ? element.maxLength : null,
      label: [
        ...labels,
        element.getAttribute("aria-label") || "",
        ariaText,
        element.getAttribute("placeholder") || "",
        element.getAttribute("title") || "",
      ].join(" "),
      name: [element.name, element.id].join(" "),
      type: element.tagName === "SELECT" ? "select" : (element.type || ""),
    }];
  });
  return JSON.stringify(out);
})()`

// Page script that asserts the origin then fills the stamped controls and reports only a count
func BuildFillJS(fills []*pyjson.Object, expectedOrigin, nonce string) string {
	payload := make([]any, 0, len(fills))
	for _, f := range fills {
		token, ok := f.Get("token")
		if !ok {
			token = "current-password"
		}
		payload = append(payload, pyjson.ObjectOf("index", anyGet(f, "index"), "token", token, "value", anyGet(f, "value")))
	}
	out := strings.ReplaceAll(fillJSTemplate, "__EXPECTED_ORIGIN__", jsonText(expectedOrigin))
	out = strings.ReplaceAll(out, "__FILLS__", jsonText(payload))
	return strings.ReplaceAll(out, "__NONCE__", jsonText(nonce))
}

const fillJSTemplate = `(() => {
  const expectedOrigin = __EXPECTED_ORIGIN__;
  if (window.location.origin !== expectedOrigin) {
    return JSON.stringify({ refused: "origin_changed", found: window.location.origin });
  }
  const fills = __FILLS__;
  const nonce = __NONCE__;
  const roots = [];
  (function walk(root) {
    roots.push(root);
    for (const e of root.querySelectorAll("*")) if (e.shadowRoot) walk(e.shadowRoot);
  })(document);
  const find = (selector) => roots.flatMap((r) => Array.from(r.querySelectorAll(selector)));
  let filled = 0;
  const norm = (t) => String(t || "").trim().toLowerCase();
  for (const f of fills) {
    const el = find('[data-icebath-vault-slot="' + nonce + ':' + f.index + '"]')[0];
    if (!el || (f.token === "current-password" && el.type !== "password")) continue;
    try {
      if (el.tagName === "SELECT") {
        const want = norm(f.value);
        const opt = Array.from(el.options).find((o) => [o.value, o.textContent].some((t) => norm(t) === want || norm(t) === want.replace(/^0/, "")));
        if (opt) { el.value = opt.value; el.dispatchEvent(new Event("change", { bubbles: true, composed: true })); filled += 1; }
        continue;
      }
      el.focus();
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value");
      if (setter && setter.set) { setter.set.call(el, f.value); } else { el.value = f.value; }
      el.dispatchEvent(new InputEvent("input", { bubbles: true, composed: true, inputType: "insertText" }));
      el.dispatchEvent(new Event("change", { bubbles: true, composed: true }));
      if (el.value.length > 0) filled += 1;
    } catch (e) { /* skip */ }
  }
  find("[data-icebath-vault-slot]").forEach((n) => n.removeAttribute("data-icebath-vault-slot"));
  return JSON.stringify({ filled });
})()`
