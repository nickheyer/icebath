package vault

import (
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Autocomplete tokens a login fill targets
var loginTokens = []string{"username", "email", "tel", "current-password"}

// Payment then address autocomplete tokens a checkout fill targets
var checkoutTokens = []string{"cc-number", "cc-name", "cc-exp", "cc-exp-month", "cc-exp-year", "cc-csc",
	"address-line1", "address-line2", "address-level2", "address-level1", "postal-code", "country-name", "country"}

// Name and label heuristics for checkout controls without an autocomplete token, first match winning
var checkoutHeuristics = []struct {
	pattern *regexp.Regexp
	token   string
}{
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

// Label and name patterns of the login and one time code heuristics over normalized text
var (
	excludedPassword = regexp.MustCompile(`\b(?:new|confirm|create|repeat)\s*password\b`)
	emailText        = regexp.MustCompile(`\b(?:e[\s-]?mail|email address)\b`)
	telText          = regexp.MustCompile(`\b(?:phone|telephone|mobile)\b`)
	usernameText     = regexp.MustCompile(`\b(?:user\s*name|username|login|account|member|membership|mileageplus)\b`)
	otpText          = regexp.MustCompile(`\b(?:one[\s-]?time|verification|security|auth(?:entication|enticator)?|2fa|two[\s-]?factor|mfa|totp|otp|passcode|sms)\b.*\b(?:code|pin|token)\b|\b(?:otp|totp|2fa|mfa|verification\s*code|passcode)\b`)
	nonAlphanumeric  = regexp.MustCompile(`[^a-z0-9]+`)
)

// Visible input control as the inspection script reports it
type Control struct {
	Autocomplete string
	FormIndex    int
	InForm       bool
	Index        int
	Label        string
	Name         string
	Type         string
	MaxLength    int
}

// Control with the fill token it was classified as and the confidence of that match
type Classified struct {
	Control Control
	Score   int
	Token   string
}

// One control a fill writes, its value inside a closure no printing path can open
type Fill struct {
	Index int
	Token string
	value func() string
}

// Fill of the control at index for token writing value
func newFill(index int, token, value string) Fill {
	return Fill{Index: index, Token: token, value: seal(value)}
}

// Value the fill writes, empty for the zero fill
func (f Fill) text() string { return unseal(f.value) }

// Writes the index and token with a redaction marker in place of the value
func (f Fill) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "{"+strconv.Itoa(f.Index)+" "+f.Token+" "+redacted+"}")
}

// Index and token with a redaction marker for structured logs
func (f Fill) LogValue() slog.Value {
	return slog.GroupValue(slog.Int("index", f.Index), slog.String("token", f.Token), slog.String("value", redacted))
}

// Index and token with a redaction marker as a JSON object
func (f Fill) MarshalJSON() ([]byte, error) {
	return json.Marshal(fillWire{Index: f.Index, Token: f.Token, Value: redacted})
}

// Error for an inspection report whose control carries a position that is not an integer
var errControlNumber = errors.New("inspection reported a control position that is not an integer")

// Controls of the inspection output, a string wrapped list unwrapped once, non-object entries skipped, false when no list
func ParseControls(data []byte) ([]Control, bool, error) {
	raw, err := parseValue(data)
	if err != nil {
		return nil, false, nil
	}
	if raw.Kind() == jsontext.KindString {
		if raw, err = parseValue([]byte(valueText(raw))); err != nil {
			return nil, false, nil
		}
	}
	items, ok := arrayItems(raw)
	if !ok {
		return nil, false, nil
	}
	var out []Control
	for _, item := range items {
		ms, ok := objectMembers(item)
		if !ok {
			continue
		}
		c, err := controlOf(ms)
		if err != nil {
			return nil, true, err
		}
		out = append(out, c)
	}
	return out, true, nil
}

// Control of one reported object, camel case names first and snake case ones second
func controlOf(ms []member) (Control, error) {
	text := func(name string) string { return truthyText(lookup(ms, name)) }
	either := func(camel, snake string) jsontext.Value {
		if v := lookup(ms, camel); v != nil {
			return v
		}
		return lookup(ms, snake)
	}
	c := Control{Autocomplete: text("autocomplete"), Label: text("label"), Name: text("name"), Type: text("type")}
	if form := either("formIndex", "form_index"); form != nil && form.Kind() != jsontext.KindNull {
		n, err := integerOf(form)
		if err != nil {
			return Control{}, err
		}
		c.FormIndex, c.InForm = n, true
	}
	if index := lookup(ms, "index"); truthy(index) {
		n, err := integerOf(index)
		if err != nil {
			return Control{}, err
		}
		c.Index = n
	}
	if length := either("maxLength", "max_length"); length != nil && length.Kind() != jsontext.KindNull {
		n, err := integerOf(length)
		if err != nil {
			return Control{}, err
		}
		c.MaxLength = n
	}
	return c, nil
}

// Integer of a number truncated toward zero, of a decimal string or of a boolean
func integerOf(v jsontext.Value) (int, error) {
	switch v.Kind() {
	case jsontext.KindTrue:
		return 1, nil
	case jsontext.KindFalse:
		return 0, nil
	case jsontext.KindString:
		if n, ok := parseInteger(valueText(v)); ok && n.IsInt64() {
			return int(n.Int64()), nil
		}
	case jsontext.KindNumber:
		if n, ok := new(big.Int).SetString(string(v), 10); ok && n.IsInt64() {
			return int(n.Int64()), nil
		}
		if f, err := strconv.ParseFloat(string(v), 64); err == nil && math.Abs(f) < 1<<62 {
			return int(f), nil
		}
	}
	return 0, errControlNumber
}

// Lowercase ASCII words of the decomposed text, everything else collapsed to single spaces
func normalizeText(value string) string {
	return strings.TrimSpace(nonAlphanumeric.ReplaceAllString(lowerFull(norm.NFKD.String(value)), " "))
}

// Name and label of a control joined and normalized
func searchable(c Control) string {
	var parts []string
	for _, p := range []string{c.Name, c.Label} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return normalizeText(strings.Join(parts, " "))
}

// Lowercased whitespace separated autocomplete tokens of a control
func autocompleteTokens(c Control) []string {
	return strings.FieldsFunc(lowerFull(c.Autocomplete), isSpace)
}

// Login fill target a control is, exact autocomplete 100, password 90, email or tel 85, heuristics 70 to 75
func ClassifyLogin(c Control) (Classified, bool) {
	tokens := autocompleteTokens(c)
	if slices.Contains(tokens, "new-password") || slices.Contains(tokens, "one-time-code") {
		return Classified{}, false
	}
	for _, token := range loginTokens {
		if slices.Contains(tokens, token) {
			return Classified{c, 100, token}, true
		}
	}
	text := searchable(c)
	switch {
	case excludedPassword.MatchString(text):
		return Classified{}, false
	case c.Type == "password":
		return Classified{c, 90, "current-password"}, true
	case c.Type == "email":
		return Classified{c, 85, "email"}, true
	case c.Type == "tel":
		return Classified{c, 85, "tel"}, true
	case emailText.MatchString(text):
		return Classified{c, 75, "email"}, true
	case telText.MatchString(text):
		return Classified{c, 75, "tel"}, true
	case usernameText.MatchString(text):
		return Classified{c, 70, "username"}, true
	}
	return Classified{}, false
}

// Controls taking a second factor code, an exact one-time-code token first and code like names second, in DOM order
func ClassifyOTP(controls []Control) []Classified {
	var out []Classified
	for _, c := range controls {
		if slices.Contains(autocompleteTokens(c), "one-time-code") {
			out = append(out, Classified{c, 100, "one-time-code"})
			continue
		}
		if !slices.Contains([]string{"text", "tel", "number", "password", ""}, c.Type) {
			continue
		}
		if otpText.MatchString(searchable(c)) {
			out = append(out, Classified{c, 70, "one-time-code"})
		}
	}
	return out
}

// Payment or address fill target a control is, exact autocomplete 100 and name or label heuristics 70
func ClassifyCheckout(c Control) (Classified, bool) {
	tokens := autocompleteTokens(c)
	for _, token := range checkoutTokens {
		if slices.Contains(tokens, token) {
			if token == "country" {
				token = "country-name"
			}
			return Classified{c, 100, token}, true
		}
	}
	if c.Type == "password" || c.Type == "email" {
		return Classified{}, false
	}
	text := searchable(c)
	for _, h := range checkoutHeuristics {
		if h.pattern.MatchString(text) {
			return Classified{c, 70, h.token}, true
		}
	}
	return Classified{}, false
}

// Best candidate for token, highest score and then earliest in DOM order
func bestFor(classified []Classified, token string) (Classified, bool) {
	var best Classified
	found := false
	for _, c := range classified {
		if c.Token != token {
			continue
		}
		if !found || c.Score > best.Score || c.Score == best.Score && c.Control.Index < best.Control.Index {
			best, found = c, true
		}
	}
	return best, found
}

// The single best current password control filled with the secret's password, none without a password field
func SelectPasswordFill(classified []Classified, secret Secret) []Fill {
	password := secret.Reveal(PasswordField)
	best, ok := bestFor(classified, "current-password")
	if !ok || password == "" {
		return nil
	}
	return []Fill{newFill(best.Control.Index, "current-password", password)}
}

// Payment or address values on the best control per token, a combined expiry replacing month and year
func SelectCheckoutFills(classified []Classified, secret Secret, kind string) []Fill {
	type tokenValue struct{ token, value string }
	var values []tokenValue
	put := func(token, value string) {
		for i := range values {
			if values[i].token == token {
				values[i].value = value
				return
			}
		}
		values = append(values, tokenValue{token, value})
	}
	valueOf := func(token string) (string, bool) {
		for _, v := range values {
			if v.token == token {
				return v.value, true
			}
		}
		return "", false
	}
	for _, f := range KindFields(kind) {
		if v := secret.Reveal(f.Name); v != "" {
			put(f.Token, v)
		}
	}
	month, hasMonth := valueOf("cc-exp-month")
	year, hasYear := valueOf("cc-exp-year")
	if hasMonth && hasYear {
		put("cc-exp", zeroPad(month, 2)+"/"+tail(year, 2))
	}
	var fills []Fill
	combined := false
	for _, v := range values {
		if best, ok := bestFor(classified, v.token); ok {
			fills = append(fills, newFill(best.Control.Index, v.token, v.value))
			combined = combined || v.token == "cc-exp"
		}
	}
	if combined {
		fills = slices.DeleteFunc(fills, func(f Fill) bool { return f.Token == "cc-exp-month" || f.Token == "cc-exp-year" })
	}
	return fills
}

// Text left padded with zeros to width runes, after a leading sign when there is one
func zeroPad(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	pad := strings.Repeat("0", width-n)
	if s != "" && (s[0] == '+' || s[0] == '-') {
		return s[:1] + pad + s[1:]
	}
	return pad + s
}

// Last n runes of s
func tail(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}

// One fill per box for an unmistakable single digit widget, else the whole code into the best scoring field
func OTPFills(otp []Classified, code string) []Fill {
	if len(otp) == 0 {
		return nil
	}
	best := otp[0]
	for _, c := range otp[1:] {
		if c.Score > best.Score {
			best = c
		}
	}
	var boxes []Classified
	for _, c := range otp {
		if c.Control.MaxLength == 1 {
			boxes = append(boxes, c)
		}
	}
	slices.SortStableFunc(boxes, func(a, b Classified) int { return a.Control.Index - b.Control.Index })
	digits := []rune(code)
	if len(boxes) == len(digits) && sameForm(boxes) && adjacent(boxes) {
		fills := make([]Fill, len(boxes))
		for i, b := range boxes {
			fills[i] = newFill(b.Control.Index, "one-time-code", string(digits[i]))
		}
		return fills
	}
	return []Fill{newFill(best.Control.Index, "one-time-code", code)}
}

// Reports whether there are boxes and every one sits in one form, all outside any form counting as one
func sameForm(boxes []Classified) bool {
	if len(boxes) == 0 {
		return false
	}
	for _, b := range boxes[1:] {
		if b.Control.InForm != boxes[0].Control.InForm || b.Control.FormIndex != boxes[0].Control.FormIndex {
			return false
		}
	}
	return true
}

// Reports whether consecutive boxes are neighbours in DOM order
func adjacent(boxes []Classified) bool {
	for i := 1; i < len(boxes); i++ {
		if boxes[i].Control.Index-boxes[i-1].Control.Index != 1 {
			return false
		}
	}
	return true
}
