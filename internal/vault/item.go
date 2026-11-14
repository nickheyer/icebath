package vault

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Marker a scrubbed or printed secret shows in place of its value
const redacted = "[REDACTED]"

// Secret field names a login password and a one time code are released under
const (
	PasswordField = "password"
	CodeField     = "code"
)

// Password echoes such as password=hunter2 in error text, whitespace as the reference reader defines it
var passwordEcho = regexp.MustCompile(`(password['"]?[\t\n\v\f\r\x1c-\x20\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]*[:=][\t\n\v\f\r\x1c-\x20\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]*)[^\t\n\v\f\r\x1c-\x20\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]+`)

// Item kinds the store accepts
func Kinds() []string { return []string{"login", "payment", "address"} }

// Identifier types a login item accepts
func IdentifierTypes() []string { return []string{"email", "phone", "username"} }

// One payload field of a payment or address item with the autocomplete token it fills
type KindField struct {
	Name     string
	Token    string
	Required bool
}

// Payload fields of a payment or address item in their canonical order, nil for any other kind
func KindFields(kind string) []KindField {
	switch kind {
	case "payment":
		return []KindField{{"card_number", "cc-number", true}, {"cardholder_name", "cc-name", false},
			{"exp_month", "cc-exp-month", true}, {"exp_year", "cc-exp-year", true}, {"cvc", "cc-csc", true},
			{"billing_postal_code", "postal-code", false}}
	case "address":
		return []KindField{{"address_line1", "address-line1", true}, {"address_line2", "address-line2", false},
			{"city", "address-level2", true}, {"state", "address-level1", false}, {"postal_code", "postal-code", true},
			{"country", "country-name", true}}
	}
	return nil
}

// Text with each value of three or more runes it contains replaced by a marker, then every password echo masked
func scrub(text string, values ...string) string {
	out := text
	for _, v := range values {
		if utf8.RuneCountInString(v) >= 3 && strings.Contains(out, v) {
			out = strings.ReplaceAll(out, v, redacted)
		}
	}
	return passwordEcho.ReplaceAllString(out, "${1}"+redacted)
}

// One named string value of a secret
type secretField struct {
	name  string
	value string
}

// Closure handing back v, the only form a struct of this package keeps a secret in
func seal[T any](v T) func() T { return func() T { return v } }

// Value a closure made by seal holds, the zero value for a nil closure
func unseal[T any](f func() T) T {
	if f == nil {
		var zero T
		return zero
	}
	return f()
}

// Decrypted secret fields inside a closure no formatter, encoder or reflective walker can open
type Secret struct {
	read func() []secretField
}

// Secret holding the given values in order, a repeated name keeping its first position and last value
func secretOf(pairs ...secretField) Secret {
	var list []secretField
	for _, p := range pairs {
		if i := slices.IndexFunc(list, func(f secretField) bool { return f.name == p.name }); i >= 0 {
			list[i].value = p.value
			continue
		}
		list = append(list, p)
	}
	return Secret{read: seal(list)}
}

// Fields of the secret, none for the zero value
func (s Secret) fields() []secretField { return unseal(s.read) }

// Value of field released for a page fill or a redaction registration, empty when absent
func (s Secret) Reveal(field string) string {
	for _, f := range s.fields() {
		if f.name == field {
			return f.value
		}
	}
	return ""
}

// Names of the fields in stored order
func (s Secret) Fields() []string {
	out := make([]string, 0, len(s.fields()))
	for _, f := range s.fields() {
		out = append(out, f.name)
	}
	return out
}

// Text with every field value of three or more runes and every password echo masked
func (s Secret) Scrub(text string) string {
	values := make([]string, 0, len(s.fields()))
	for _, f := range s.fields() {
		values = append(values, f.value)
	}
	return scrub(text, values...)
}

// Redaction marker in place of the values
func (Secret) String() string { return redacted }

// Writes the redaction marker for every format verb
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// Redaction marker for structured logs
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// Redaction marker as a JSON string
func (Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// Secret fields an item is added with, in the order supplied, inside a closure no printing path can open
type Payload struct {
	read func() []member
}

// One named string of a payload or plugin secret, its value inside a closure no printing path can open
type Field struct {
	Name  string
	value func() string
}

// Field carrying value under name
func NewField(name, value string) Field { return Field{Name: name, value: seal(value)} }

// Value the field carries, empty for the zero field
func (f Field) text() string { return unseal(f.value) }

// Writes the name with a redaction marker in place of the value
func (f Field) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, "{"+f.Name+" "+redacted+"}") }

// Name with a redaction marker for structured logs
func (f Field) LogValue() slog.Value {
	return slog.GroupValue(slog.String("name", f.Name), slog.String("value", redacted))
}

// Name with a redaction marker as a JSON object
func (f Field) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"name": f.Name, "value": redacted})
}

// Payload of string fields, a repeated name keeping its first position and last value
func NewPayload(fields ...Field) Payload {
	var list []member
	for _, f := range fields {
		list = setMember(list, f.Name, stringValue(f.text()))
	}
	return Payload{read: seal(list)}
}

// Payload of a JSON object sent over the local RPC channel, field order and value types kept
func ParsePayload(data []byte) (Payload, error) {
	v, err := parseValue(data)
	if err != nil {
		return Payload{}, failure(ErrInvalid, "secret payload must be a JSON object")
	}
	list, ok := objectMembers(v)
	if !ok {
		return Payload{}, failure(ErrInvalid, "secret payload must be a JSON object")
	}
	return Payload{read: seal(list)}, nil
}

// Copy of the payload entries
func (p Payload) members() []member { return slices.Clone(unseal(p.read)) }

// Fields the payload holds
func (p Payload) Len() int { return len(p.members()) }

// Writes the redaction marker for every format verb
func (Payload) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// Redaction marker for structured logs
func (Payload) LogValue() slog.Value { return slog.StringValue(redacted) }

// Redaction marker as a JSON string
func (Payload) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// Text with every string field value of three or more runes and every password echo masked
func (p Payload) Scrub(text string) string {
	var values []string
	for _, m := range p.members() {
		if m.value.Kind() == jsontext.KindString {
			values = append(values, valueText(m.value))
		}
	}
	return scrub(text, values...)
}

// Metadata of a vault item, never a secret value, the login identifier being metadata the agent may type
type ItemMeta struct {
	ID             string
	Kind           string
	Label          string
	Origin         string
	CreatedAt      string
	IdentifierType string
	Identifier     string
	HasOTP         bool
	AllowedOrigins []string
}

// Exact origins the item may fill, every bound manager origin or else the single saved origin
func (m ItemMeta) SavedOrigins() []string {
	if len(m.AllowedOrigins) > 0 {
		return slices.Clone(m.AllowedOrigins)
	}
	if m.Origin != "" {
		return []string{m.Origin}
	}
	return nil
}

// Reports whether origin equals one of the saved origins, no wildcard or parent domain ever inferred
func (m ItemMeta) Binds(origin string) bool { return slices.Contains(m.SavedOrigins(), origin) }

// Wire object with the identifier pair, OTP flag and origin list present only when set
func (m ItemMeta) MarshalJSON() ([]byte, error) {
	type wire struct {
		ID             string   `json:"id"`
		Kind           string   `json:"kind"`
		Label          string   `json:"label"`
		Origin         *string  `json:"origin"`
		CreatedAt      string   `json:"created_at"`
		Identifier     *string  `json:"identifier,omitempty"`
		IdentifierType any      `json:"identifier_type,omitempty"`
		HasOTP         bool     `json:"has_otp,omitempty"`
		AllowedOrigins []string `json:"allowed_origins,omitempty"`
	}
	w := wire{ID: m.ID, Kind: m.Kind, Label: m.Label, CreatedAt: m.CreatedAt, HasOTP: m.HasOTP}
	if m.Origin != "" {
		w.Origin = &m.Origin
	}
	if m.Identifier != "" {
		w.Identifier = &m.Identifier
		w.IdentifierType = json.RawMessage("null")
		if m.IdentifierType != "" {
			w.IdentifierType = m.IdentifierType
		}
	}
	if len(m.AllowedOrigins) > 1 {
		w.AllowedOrigins = m.AllowedOrigins
	}
	return json.Marshal(w)
}
