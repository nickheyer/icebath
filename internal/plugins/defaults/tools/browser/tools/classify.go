package tools

import (
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cast"
	"golang.org/x/text/unicode/norm"

	"github.com/nickheyer/icebath/internal/config"
)

var (
	loginTokens   = []string{"username", "email", "tel", "current-password"}
	paymentTokens = []string{"cc-number", "cc-name", "cc-exp", "cc-exp-month", "cc-exp-year", "cc-csc"}
	addressTokens = []string{"address-line1", "address-line2", "address-level2", "address-level1", "postal-code", "country-name", "country"}
)

type heuristic struct {
	pattern *regexp.Regexp
	token   string
}

var checkoutHeuristics = []heuristic{
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
	newPassword = regexp.MustCompile(`\b(?:new|confirm|create|repeat)\s*password\b`)
	emailText   = regexp.MustCompile(`\b(?:e[\s-]?mail|email address)\b`)
	phoneText   = regexp.MustCompile(`\b(?:phone|telephone|mobile)\b`)
	userText    = regexp.MustCompile(`\b(?:user\s*name|username|login|account|member|membership|mileageplus)\b`)
	codeText    = regexp.MustCompile(`\b(?:one[\s-]?time|verification|security|auth(?:entication|enticator)?|2fa|two[\s-]?factor|mfa|totp|otp|` +
		`passcode|sms)\b.*\b(?:code|pin|token)\b|\b(?:otp|totp|2fa|mfa|verification\s*code|passcode)\b`)
	nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
)

// Compatibility decomposed, lower case, every other run one space
func normalizeText(value string) string {
	return strings.TrimSpace(nonAlnum.ReplaceAllString(strings.ToLower(norm.NFKD.String(value)), " "))
}

// A visible input of the page as the inspection script reports it
type control struct {
	Autocomplete string
	Form         *int
	Index        int
	Label        string
	Name         string
	Type         string
	MaxLength    *int
}

func optionalInt(raw map[string]any, camel, snake string) (*int, error) {
	v, ok := raw[camel]
	if !ok {
		v = raw[snake]
	}
	if v == nil {
		return nil, nil
	}
	n, err := cast.ToIntE(v)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func text(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return cast.ToString(v)
}

// Control of one record of the inspection, either spelling of its keys taken
func controlOf(raw map[string]any) (control, error) {
	form, err := optionalInt(raw, "formIndex", "form_index")
	if err != nil {
		return control{}, err
	}
	maxLength, err := optionalInt(raw, "maxLength", "max_length")
	if err != nil {
		return control{}, err
	}
	index := 0
	if v := raw["index"]; config.Truthy(v) {
		if index, err = cast.ToIntE(v); err != nil {
			return control{}, err
		}
	}
	return control{Autocomplete: text(raw["autocomplete"]), Form: form, Index: index, Label: text(raw["label"]), Name: text(raw["name"]),
		Type: text(raw["type"]), MaxLength: maxLength}, nil
}

type classified struct {
	control control
	score   int
	token   string
}

func searchable(c control) string {
	var parts []string
	for _, part := range []string{c.Name, c.Label} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return normalizeText(strings.Join(parts, " "))
}

// A control as a login fill target, nil for none
func classifyLogin(c control) *classified {
	tokens := strings.Fields(strings.ToLower(c.Autocomplete))
	if slices.Contains(tokens, "new-password") || slices.Contains(tokens, "one-time-code") {
		return nil
	}
	for _, token := range loginTokens {
		if slices.Contains(tokens, token) {
			return &classified{c, 100, token}
		}
	}
	s := searchable(c)
	if newPassword.MatchString(s) {
		return nil
	}
	switch c.Type {
	case "password":
		return &classified{c, 90, "current-password"}
	case "email":
		return &classified{c, 85, "email"}
	case "tel":
		return &classified{c, 85, "tel"}
	}
	switch {
	case emailText.MatchString(s):
		return &classified{c, 75, "email"}
	case phoneText.MatchString(s):
		return &classified{c, 75, "tel"}
	case userText.MatchString(s):
		return &classified{c, 70, "username"}
	}
	return nil
}

// Controls taking a second factor code in page order, autocomplete one-time-code deciding
func classifyCodes(controls []control) []classified {
	var out []classified
	for _, c := range controls {
		if slices.Contains(strings.Fields(strings.ToLower(c.Autocomplete)), "one-time-code") {
			out = append(out, classified{c, 100, "one-time-code"})
			continue
		}
		switch c.Type {
		case "text", "tel", "number", "password", "":
		default:
			continue
		}
		if codeText.MatchString(searchable(c)) {
			out = append(out, classified{c, 70, "one-time-code"})
		}
	}
	return out
}

// Highest score first, page order breaking ties
func best(candidates []classified) classified {
	top := candidates[0]
	for _, c := range candidates[1:] {
		if c.score > top.score || c.score == top.score && c.control.Index < top.control.Index {
			top = c
		}
	}
	return top
}

func withToken(all []classified, token string) []classified {
	var out []classified
	for _, c := range all {
		if c.token == token {
			out = append(out, c)
		}
	}
	return out
}

// One value written into one stamped control
type fill struct {
	Index int    `json:"index"`
	Token string `json:"token"`
	Value string `json:"value"`
}

// The fill of the best current-password control, none without one or without a password
func passwordFill(all []classified, password string) []fill {
	passwords := withToken(all, "current-password")
	if len(passwords) == 0 || password == "" {
		return nil
	}
	return []fill{{best(passwords).control.Index, "current-password", password}}
}

// A control as a payment or address fill target; password and email inputs never match by their text
func classifyCheckout(c control) *classified {
	tokens := strings.Fields(strings.ToLower(c.Autocomplete))
	for _, token := range append(slices.Clone(paymentTokens), addressTokens...) {
		if slices.Contains(tokens, token) {
			if token == "country" {
				token = "country-name"
			}
			return &classified{c, 100, token}
		}
	}
	if c.Type == "password" || c.Type == "email" {
		return nil
	}
	s := searchable(c)
	for _, h := range checkoutHeuristics {
		if h.pattern.MatchString(s) {
			return &classified{c, 70, h.token}
		}
	}
	return nil
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

// A payment or address item laid on the best control of each token, a combined expiry taking the place of month and year
func checkoutFills(all []classified, secret map[string]string, fields [][2]string) []fill {
	var order []string
	values := map[string]string{}
	set := func(token, value string) {
		if _, seen := values[token]; !seen {
			order = append(order, token)
		}
		values[token] = value
	}
	for _, field := range fields {
		if secret[field[0]] != "" {
			set(field[1], secret[field[0]])
		}
	}
	month, hasMonth := values["cc-exp-month"]
	year, hasYear := values["cc-exp-year"]
	if hasMonth && hasYear {
		r := []rune(year)
		set("cc-exp", zfill2(month)+"/"+string(r[max(len(r)-2, 0):]))
	}
	var fills []fill
	combined := false
	for _, token := range order {
		if candidates := withToken(all, token); len(candidates) > 0 {
			fills = append(fills, fill{best(candidates).control.Index, token, values[token]})
			combined = combined || token == "cc-exp"
		}
	}
	if !combined {
		return fills
	}
	return slices.DeleteFunc(fills, func(f fill) bool { return f.Token == "cc-exp-month" || f.Token == "cc-exp-year" })
}

// One fill per single character box of one form side by side, else the best field takes the whole code
func codeFills(codes []classified, code string) []fill {
	top := codes[0]
	for _, c := range codes[1:] {
		if c.score > top.score {
			top = c
		}
	}
	var boxes []classified
	for _, c := range codes {
		if c.control.MaxLength != nil && *c.control.MaxLength == 1 {
			boxes = append(boxes, c)
		}
	}
	sort.SliceStable(boxes, func(i, j int) bool { return boxes[i].control.Index < boxes[j].control.Index })
	digits := []rune(code)
	if len(boxes) == len(digits) && adjacent(boxes) {
		out := make([]fill, len(boxes))
		for i, b := range boxes {
			out[i] = fill{b.control.Index, "one-time-code", string(digits[i])}
		}
		return out
	}
	return []fill{{top.control.Index, "one-time-code", code}}
}

// Whether the boxes share one form and follow each other in page order
func adjacent(boxes []classified) bool {
	type key struct {
		grouped bool
		index   int
	}
	forms := map[key]bool{}
	for _, b := range boxes {
		k := key{}
		if b.control.Form != nil {
			k = key{true, *b.control.Form}
		}
		forms[k] = true
	}
	if len(forms) != 1 {
		return false
	}
	for i := 1; i < len(boxes); i++ {
		if boxes[i].control.Index-boxes[i-1].control.Index != 1 {
			return false
		}
	}
	return true
}

func jsonText(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// Page script that stamps every input with the nonce and reports the visible ones
func inspectionJS(nonce string) string {
	return strings.ReplaceAll(inspectionTemplate, "__NONCE__", jsonText(nonce))
}

const inspectionTemplate = `(() => {
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

// Page script that checks the origin, fills the stamped controls and reports only how many
func fillJS(fills []fill, origin, nonce string) string {
	payload := make([]fill, len(fills))
	for i, f := range fills {
		if f.Token == "" {
			f.Token = "current-password"
		}
		payload[i] = f
	}
	out := strings.ReplaceAll(fillTemplate, "__EXPECTED_ORIGIN__", jsonText(origin))
	out = strings.ReplaceAll(out, "__FILLS__", jsonText(payload))
	return strings.ReplaceAll(out, "__NONCE__", jsonText(nonce))
}

const fillTemplate = `(() => {
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
