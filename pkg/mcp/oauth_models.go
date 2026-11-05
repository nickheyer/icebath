package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// jsonObject is a decoded JSON object that keeps its key order across a rewrite
type jsonObject struct {
	keys []string
	vals map[string]json.RawMessage
}

// Empty ordered object
func newJSONObject() *jsonObject { return &jsonObject{vals: map[string]json.RawMessage{}} }

// Decodes a JSON document, objects at the top level kept ordered, numbers kept exact
func decodeJSONDocument(data []byte) (any, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '{' {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		obj := newJSONObject()
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, _ := tok.(string)
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return nil, err
			}
			obj.setRaw(key, raw)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		if _, err := dec.Token(); err == nil {
			return nil, fmt.Errorf("Extra data")
		}
		return obj, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("Extra data")
	}
	return v, nil
}

// Decodes one raw value with exact numbers
func decodeRawValue(raw json.RawMessage) any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	return v
}

// Sets a key to raw JSON, appending it on first use
func (o *jsonObject) setRaw(key string, raw json.RawMessage) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = raw
}

// Sets a key to the JSON encoding of v
func (o *jsonObject) set(key string, v any) {
	o.setRaw(key, json.RawMessage(encodeJSONValue(v)))
}

// Removes a key, reporting whether it was present
func (o *jsonObject) pop(key string) (any, bool) {
	raw, ok := o.vals[key]
	if !ok {
		return nil, false
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
	return decodeRawValue(raw), true
}

// Decoded value of a key, nil when absent or null
func (o *jsonObject) get(key string) any {
	raw, ok := o.vals[key]
	if !ok {
		return nil
	}
	return decodeRawValue(raw)
}

// Plain map of every member with exact numbers, the shape model validation reads
func (o *jsonObject) toMap() map[string]any {
	m := make(map[string]any, len(o.keys))
	for _, k := range o.keys {
		m[k] = decodeRawValue(o.vals[k])
	}
	return m
}

// Compact JSON with keys in order
func (o *jsonObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(encodeJSONValue(k))
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// JSON of a value without HTML escaping, the way json.dumps with ensure_ascii off writes it
func encodeJSONValue(v any) []byte {
	if f, ok := v.(float64); ok && !math.IsInf(f, 0) && !math.IsNaN(f) {
		return []byte(pyFloatRepr(f))
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimRight(b.Bytes(), "\n")
}

// Two space indented JSON, the layout json.dump with indent 2 produces
func indentJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Python truthiness of a decoded JSON value
func jsonTruthy(v any) bool {
	switch x := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		return err != nil || f != 0
	case *jsonObject:
		return len(x.keys) > 0
	}
	return pyTruthy(v)
}

// Python type name of a decoded JSON document
func pyJSONTypeName(v any) string {
	switch x := v.(type) {
	case *jsonObject, map[string]any:
		return "dict"
	case []any:
		return "list"
	case string:
		return "str"
	case bool:
		return "bool"
	case json.Number:
		if strings.ContainsAny(string(x), ".eE") {
			return "float"
		}
		return "int"
	case nil:
		return "NoneType"
	}
	return fmt.Sprintf("%T", v)
}

// FieldError is one failing field of a model validation
type FieldError struct {
	Loc, Msg, Type string
}

// ValidationError lists the fields a model rejected without echoing their input
type ValidationError struct {
	Model  string
	Errors []FieldError
}

func (e *ValidationError) Error() string {
	plural := "s"
	if len(e.Errors) == 1 {
		plural = ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d validation error%s for %s", len(e.Errors), plural, e.Model)
	for _, fe := range e.Errors {
		fmt.Fprintf(&b, "\n%s\n  %s [type=%s]", fe.Loc, fe.Msg, fe.Type)
	}
	return b.String()
}

// Comma joined locations of every failing field
func (e *ValidationError) Fields() string {
	locs := make([]string, 0, len(e.Errors))
	for _, fe := range e.Errors {
		locs = append(locs, fe.Loc)
	}
	return strings.Join(locs, ", ")
}

// modelReader validates members of a decoded object with pydantic's lax JSON coercions
type modelReader struct {
	m    map[string]any
	errs []FieldError
}

// Records a field failure
func (r *modelReader) fail(loc, msg, typ string) { r.errs = append(r.errs, FieldError{loc, msg, typ}) }

// Validation error for the model, nil when every field passed
func (r *modelReader) err(model string) error {
	if len(r.errs) == 0 {
		return nil
	}
	return &ValidationError{Model: model, Errors: r.errs}
}

// Member value and whether the key is present at all
func (r *modelReader) member(key string) (any, bool) {
	v, ok := r.m[key]
	return v, ok
}

// Required or optional string member, nil when absent or null on an optional field
func (r *modelReader) str(key string, required bool) *string {
	v, ok := r.member(key)
	if !ok {
		if required {
			r.fail(key, "Field required", "missing")
		}
		return nil
	}
	if v == nil && !required {
		return nil
	}
	s, isStr := v.(string)
	if !isStr {
		r.fail(key, "Input should be a valid string", "string_type")
		return nil
	}
	return &s
}

// Lax integer coercion of one value, ok false with the pydantic error when it fails
func laxInt(v any) (int64, string, string, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, "", "", true
		}
		return 0, "", "", true
	case json.Number:
		if n, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return n, "", "", true
		}
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return 0, "Input should be a valid integer", "int_parsing", false
		}
		if f != math.Trunc(f) {
			return 0, "Input should be a valid integer, got a number with a fractional part", "int_from_float", false
		}
		return int64(f), "", "", true
	case string:
		s := strings.TrimSpace(x)
		if n, err := strconv.ParseInt(strings.TrimPrefix(s, "+"), 10, 64); err == nil && !strings.HasPrefix(s, "+-") {
			return n, "", "", true
		}
		return 0, "Input should be a valid integer, unable to parse string as an integer", "int_parsing", false
	}
	return 0, "Input should be a valid integer", "int_type", false
}

// Optional integer member
func (r *modelReader) integer(key string) *int64 {
	v, ok := r.member(key)
	if !ok || v == nil {
		return nil
	}
	n, msg, typ, good := laxInt(v)
	if !good {
		r.fail(key, msg, typ)
		return nil
	}
	return &n
}

// Optional boolean member with pydantic's accepted spellings
func (r *modelReader) boolean(key string) *bool {
	v, ok := r.member(key)
	if !ok || v == nil {
		return nil
	}
	t, f := true, false
	switch x := v.(type) {
	case bool:
		return &x
	case json.Number:
		switch string(x) {
		case "0", "0.0":
			return &f
		case "1", "1.0":
			return &t
		}
		r.fail(key, "Input should be a valid boolean, unable to interpret input", "bool_parsing")
		return nil
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "0", "off", "f", "false", "n", "no":
			return &f
		case "1", "on", "t", "true", "y", "yes":
			return &t
		}
		r.fail(key, "Input should be a valid boolean, unable to interpret input", "bool_parsing")
		return nil
	}
	r.fail(key, "Input should be a valid boolean", "bool_type")
	return nil
}

// List of strings member, present false when absent or null on an optional field
func (r *modelReader) strList(key string, nullable bool) ([]string, bool) {
	v, ok := r.member(key)
	if !ok || (v == nil && nullable) {
		return nil, false
	}
	items, isList := v.([]any)
	if !isList {
		r.fail(key, "Input should be a valid list", "list_type")
		return nil, false
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, isStr := item.(string)
		if !isStr {
			r.fail(fmt.Sprintf("%s.%d", key, i), "Input should be a valid string", "string_type")
			continue
		}
		out = append(out, s)
	}
	return out, true
}

// URL member normalized the way pydantic serializes it, http and https only when httpOnly
func (r *modelReader) url(key string, required, httpOnly bool) *string {
	v, ok := r.member(key)
	if !ok {
		if required {
			r.fail(key, "Field required", "missing")
		}
		return nil
	}
	if v == nil && !required {
		return nil
	}
	s, isStr := v.(string)
	if !isStr {
		r.fail(key, "URL input should be a string or URL", "url_type")
		return nil
	}
	n, err := normalizeURL(s, httpOnly)
	if err != nil {
		ue := err.(*urlError)
		r.fail(key, ue.msg, ue.typ)
		return nil
	}
	return &n
}

// List of URLs member normalized element by element
func (r *modelReader) urlList(key string, httpOnly, nullable bool) ([]string, bool) {
	v, ok := r.member(key)
	if !ok || (v == nil && nullable) {
		return nil, false
	}
	items, isList := v.([]any)
	if !isList {
		r.fail(key, "Input should be a valid list", "list_type")
		return nil, false
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, isStr := item.(string)
		if !isStr {
			r.fail(fmt.Sprintf("%s.%d", key, i), "URL input should be a string or URL", "url_type")
			continue
		}
		n, err := normalizeURL(s, httpOnly)
		if err != nil {
			ue := err.(*urlError)
			r.fail(fmt.Sprintf("%s.%d", key, i), ue.msg, ue.typ)
			continue
		}
		out = append(out, n)
	}
	return out, true
}

// Raw member kept as decoded JSON, nil when absent or null
func (r *modelReader) raw(key string) any {
	v, ok := r.member(key)
	if !ok {
		return nil
	}
	return v
}

// Object members of a decoded document, false when it is not an object
func documentMap(doc any) (map[string]any, bool) {
	switch x := doc.(type) {
	case *jsonObject:
		return x.toMap(), true
	case map[string]any:
		return x, true
	}
	return nil, false
}

// Ordered object builder that skips nil members like exclude_none
type dumpBuilder struct{ o *jsonObject }

// Starts an empty dump
func newDump() *dumpBuilder { return &dumpBuilder{o: newJSONObject()} }

// Adds a string pointer member when set
func (d *dumpBuilder) str(key string, v *string) *dumpBuilder {
	if v != nil {
		d.o.set(key, *v)
	}
	return d
}

// Adds an integer pointer member when set
func (d *dumpBuilder) integer(key string, v *int64) *dumpBuilder {
	if v != nil {
		d.o.set(key, *v)
	}
	return d
}

// Adds a boolean pointer member when set
func (d *dumpBuilder) boolean(key string, v *bool) *dumpBuilder {
	if v != nil {
		d.o.set(key, *v)
	}
	return d
}

// Adds a list member when present
func (d *dumpBuilder) list(key string, v []string, present bool) *dumpBuilder {
	if present {
		if v == nil {
			v = []string{}
		}
		d.o.set(key, v)
	}
	return d
}

// Adds a raw JSON member when set
func (d *dumpBuilder) value(key string, v any) *dumpBuilder {
	if v != nil {
		d.o.set(key, v)
	}
	return d
}

func strPtr(s string) *string { return &s }

func int64Ptr(n int64) *int64 { return &n }

// Dereferenced string, empty for nil
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Python str of an optional string, None for nil
func pyOptStr(s *string) string {
	if s == nil {
		return "None"
	}
	return *s
}

// OAuthToken is the RFC 6749 token response the client stores
type OAuthToken struct {
	AccessToken  string
	TokenType    string
	ExpiresIn    *int64
	Scope        *string
	RefreshToken *string
}

// Validates a token object with the Bearer title casing pydantic applies
func validateOAuthToken(m map[string]any) (*OAuthToken, error) {
	r := &modelReader{m: m}
	t := &OAuthToken{TokenType: "Bearer"}
	if s := r.str("access_token", true); s != nil {
		t.AccessToken = *s
	}
	if v, ok := m["token_type"]; ok {
		s, isStr := v.(string)
		if isStr {
			s = pyTitle(s)
		}
		if !isStr || s != "Bearer" {
			r.fail("token_type", "Input should be 'Bearer'", "literal_error")
		} else {
			t.TokenType = s
		}
	}
	t.ExpiresIn = r.integer("expires_in")
	t.Scope = r.str("scope", false)
	t.RefreshToken = r.str("refresh_token", false)
	if err := r.err("OAuthToken"); err != nil {
		return nil, err
	}
	return t, nil
}

// Parses token JSON bytes into a validated token
func parseOAuthToken(data []byte) (*OAuthToken, error) {
	doc, err := decodeJSONDocument(data)
	if err != nil {
		return nil, &ValidationError{Model: "OAuthToken", Errors: []FieldError{{"", "Invalid JSON: " + err.Error(), "json_invalid"}}}
	}
	m, ok := documentMap(doc)
	if !ok {
		return nil, &ValidationError{Model: "OAuthToken", Errors: []FieldError{{"", "Input should be an object", "model_type"}}}
	}
	return validateOAuthToken(m)
}

// Copy of the token
func (t *OAuthToken) clone() *OAuthToken {
	c := *t
	return &c
}

// Ordered exclude_none dump of the token
func (t *OAuthToken) dump() *jsonObject {
	d := newDump()
	d.o.set("access_token", t.AccessToken)
	d.o.set("token_type", t.TokenType)
	return d.integer("expires_in", t.ExpiresIn).str("scope", t.Scope).str("refresh_token", t.RefreshToken).o
}

// Python str.title of an ASCII heavy word
func pyTitle(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		isLetter := strings.ToUpper(string(r)) != strings.ToLower(string(r))
		if isLetter {
			if prevCased {
				b.WriteString(strings.ToLower(string(r)))
			} else {
				b.WriteString(strings.ToUpper(string(r)))
			}
		} else {
			b.WriteRune(r)
		}
		prevCased = isLetter
	}
	return b.String()
}

// clientMetadataBase is the RFC 7591 metadata shared by the registration request and the stored record
type clientMetadataBase struct {
	ResponseTypes   []string
	Scope           *string
	ClientName      *string
	ClientURI       *string
	LogoURI         *string
	Contacts        []string
	HasContacts     bool
	TosURI          *string
	PolicyURI       *string
	JwksURI         *string
	Jwks            any
	SoftwareID      *string
	SoftwareVersion *string
}

// Reads the shared metadata fields, blank optional URLs read as absent
func (b *clientMetadataBase) read(r *modelReader) {
	b.ResponseTypes = []string{"code"}
	if v, ok := r.strList("response_types", false); ok {
		b.ResponseTypes = v
	}
	b.Scope = r.str("scope", false)
	b.ClientName = r.str("client_name", false)
	for _, key := range []string{"client_uri", "logo_uri", "tos_uri", "policy_uri", "jwks_uri"} {
		if r.m[key] == "" {
			delete(r.m, key)
		}
	}
	b.ClientURI = r.url("client_uri", false, true)
	b.LogoURI = r.url("logo_uri", false, true)
	b.Contacts, b.HasContacts = r.strList("contacts", true)
	b.TosURI = r.url("tos_uri", false, true)
	b.PolicyURI = r.url("policy_uri", false, true)
	b.JwksURI = r.url("jwks_uri", false, true)
	b.Jwks = r.raw("jwks")
	b.SoftwareID = r.str("software_id", false)
	b.SoftwareVersion = r.str("software_version", false)
}

// Writes the shared metadata fields in model order
func (b *clientMetadataBase) dumpInto(d *dumpBuilder) {
	d.list("response_types", b.ResponseTypes, true).str("scope", b.Scope).str("client_name", b.ClientName).
		str("client_uri", b.ClientURI).str("logo_uri", b.LogoURI).list("contacts", b.Contacts, b.HasContacts).
		str("tos_uri", b.TosURI).str("policy_uri", b.PolicyURI).str("jwks_uri", b.JwksURI).value("jwks", b.Jwks).
		str("software_id", b.SoftwareID).str("software_version", b.SoftwareVersion)
}

var defaultGrantTypes = []string{"authorization_code", "refresh_token"}

// OAuthClientMetadata is the dynamic client registration request this client sends
type OAuthClientMetadata struct {
	clientMetadataBase
	RedirectURIs            []string
	TokenEndpointAuthMethod *string
	GrantTypes              []string
	ApplicationType         string
}

var tokenEndpointAuthMethods = map[string]bool{"none": true, "client_secret_post": true, "client_secret_basic": true, "private_key_jwt": true}

// Validates registration request metadata with its literal auth method and application type
func validateClientMetadata(m map[string]any) (*OAuthClientMetadata, error) {
	r := &modelReader{m: m}
	md := &OAuthClientMetadata{GrantTypes: append([]string(nil), defaultGrantTypes...), ApplicationType: "native"}
	md.read(r)
	if _, ok := m["redirect_uris"]; !ok {
		r.fail("redirect_uris", "Field required", "missing")
	} else if uris, present := r.urlList("redirect_uris", false, true); present {
		if len(uris) == 0 {
			r.fail("redirect_uris", "List should have at least 1 item after validation, not 0", "too_short")
		}
		md.RedirectURIs = uris
	}
	if v, ok := m["token_endpoint_auth_method"]; ok && v != nil {
		s, isStr := v.(string)
		if !isStr || !tokenEndpointAuthMethods[s] {
			r.fail("token_endpoint_auth_method", "Input should be 'none', 'client_secret_post', 'client_secret_basic' or 'private_key_jwt'", "literal_error")
		} else {
			md.TokenEndpointAuthMethod = &s
		}
	}
	if v, ok := r.strList("grant_types", false); ok {
		md.GrantTypes = v
	}
	if v, ok := m["application_type"]; ok {
		s, isStr := v.(string)
		if !isStr || (s != "web" && s != "native") {
			r.fail("application_type", "Input should be 'web' or 'native'", "literal_error")
		} else {
			md.ApplicationType = s
		}
	}
	if err := r.err("OAuthClientMetadata"); err != nil {
		return nil, err
	}
	return md, nil
}

// Ordered exclude_none dump, the registration request body
func (md *OAuthClientMetadata) dump() *jsonObject {
	d := newDump()
	md.dumpInto(d)
	d.list("redirect_uris", md.RedirectURIs, md.RedirectURIs != nil).str("token_endpoint_auth_method", md.TokenEndpointAuthMethod).
		list("grant_types", md.GrantTypes, true)
	d.o.set("application_type", md.ApplicationType)
	return d.o
}

// Copy of the metadata whose scope can change without touching the original
func (md *OAuthClientMetadata) clone() *OAuthClientMetadata {
	c := *md
	return &c
}

// OAuthClientInfo is the authorization server's record of a registered client
type OAuthClientInfo struct {
	clientMetadataBase
	RedirectURIs            []string
	HasRedirectURIs         bool
	TokenEndpointAuthMethod *string
	GrantTypes              []string
	ApplicationType         *string
	ClientID                string
	ClientSecret            *string
	ClientIDIssuedAt        *int64
	ClientSecretExpiresAt   *int64
	Issuer                  *string
}

// Validates a client record, null and blank members reading as omitted
func validateClientInfo(m map[string]any) (*OAuthClientInfo, error) {
	clean := make(map[string]any, len(m))
	for k, v := range m {
		if v == nil || v == "" {
			continue
		}
		clean[k] = v
	}
	r := &modelReader{m: clean}
	ci := &OAuthClientInfo{GrantTypes: append([]string(nil), defaultGrantTypes...)}
	ci.read(r)
	ci.RedirectURIs, ci.HasRedirectURIs = r.urlList("redirect_uris", false, true)
	ci.TokenEndpointAuthMethod = r.str("token_endpoint_auth_method", false)
	if v, ok := r.strList("grant_types", false); ok {
		ci.GrantTypes = v
	}
	ci.ApplicationType = r.str("application_type", false)
	if s := r.str("client_id", true); s != nil {
		ci.ClientID = *s
	}
	ci.ClientSecret = r.str("client_secret", false)
	ci.ClientIDIssuedAt = r.integer("client_id_issued_at")
	ci.ClientSecretExpiresAt = r.integer("client_secret_expires_at")
	ci.Issuer = r.str("issuer", false)
	if err := r.err("OAuthClientInformationFull"); err != nil {
		return nil, err
	}
	return ci, nil
}

// Ordered exclude_none dump of the client record
func (ci *OAuthClientInfo) dump() *jsonObject {
	d := newDump()
	ci.dumpInto(d)
	return d.list("redirect_uris", ci.RedirectURIs, ci.HasRedirectURIs).str("token_endpoint_auth_method", ci.TokenEndpointAuthMethod).
		list("grant_types", ci.GrantTypes, true).str("application_type", ci.ApplicationType).str("client_id", &ci.ClientID).
		str("client_secret", ci.ClientSecret).integer("client_id_issued_at", ci.ClientIDIssuedAt).
		integer("client_secret_expires_at", ci.ClientSecretExpiresAt).str("issuer", ci.Issuer).o
}

// OAuthMetadata is RFC 8414 authorization server metadata, with the RFC 8628 device endpoint when advertised
type OAuthMetadata struct {
	Issuer                                             string
	AuthorizationEndpoint                              *string
	TokenEndpoint                                      string
	RegistrationEndpoint                               *string
	ScopesSupported                                    []string
	HasScopesSupported                                 bool
	ResponseTypesSupported                             []string
	ResponseModesSupported                             []string
	GrantTypesSupported                                []string
	TokenEndpointAuthMethodsSupported                  []string
	TokenEndpointAuthSigningAlgValuesSupported         []string
	ServiceDocumentation                               *string
	UILocalesSupported                                 []string
	OpPolicyURI                                        *string
	OpTosURI                                           *string
	RevocationEndpoint                                 *string
	RevocationEndpointAuthMethodsSupported             []string
	RevocationEndpointAuthSigningAlgValuesSupported    []string
	IntrospectionEndpoint                              *string
	IntrospectionEndpointAuthMethodsSupported          []string
	IntrospectionEndpointAuthSigningAlgValuesSupported []string
	CodeChallengeMethodsSupported                      []string
	ClientIDMetadataDocumentSupported                  *bool
	AuthorizationResponseIssParameterSupported         *bool
	AuthorizationGrantProfilesSupported                []string
	DeviceAuthorizationEndpoint                        *string
	present                                            map[string]bool
}

var metadataListKeys = []string{"response_modes_supported", "grant_types_supported", "token_endpoint_auth_methods_supported",
	"token_endpoint_auth_signing_alg_values_supported", "ui_locales_supported", "revocation_endpoint_auth_methods_supported",
	"revocation_endpoint_auth_signing_alg_values_supported", "introspection_endpoint_auth_methods_supported",
	"introspection_endpoint_auth_signing_alg_values_supported", "code_challenge_methods_supported",
	"authorization_grant_profiles_supported"}

// Validates authorization server metadata, the device variant when allowed and a device endpoint is named
func validateOAuthMetadata(m map[string]any, allowDevice bool) (*OAuthMetadata, error) {
	device := allowDevice && jsonTruthy(m["device_authorization_endpoint"])
	r := &modelReader{m: m}
	md := &OAuthMetadata{ResponseTypesSupported: []string{"code"}, present: map[string]bool{}}
	if s := r.url("issuer", true, true); s != nil {
		md.Issuer = *s
	}
	md.AuthorizationEndpoint = r.url("authorization_endpoint", !device, true)
	if s := r.url("token_endpoint", true, true); s != nil {
		md.TokenEndpoint = *s
	}
	md.RegistrationEndpoint = r.url("registration_endpoint", false, true)
	md.ScopesSupported, md.HasScopesSupported = r.strList("scopes_supported", true)
	if v, ok := r.strList("response_types_supported", false); ok {
		md.ResponseTypesSupported = v
	}
	lists := []*[]string{&md.ResponseModesSupported, &md.GrantTypesSupported, &md.TokenEndpointAuthMethodsSupported,
		&md.TokenEndpointAuthSigningAlgValuesSupported, &md.UILocalesSupported, &md.RevocationEndpointAuthMethodsSupported,
		&md.RevocationEndpointAuthSigningAlgValuesSupported, &md.IntrospectionEndpointAuthMethodsSupported,
		&md.IntrospectionEndpointAuthSigningAlgValuesSupported, &md.CodeChallengeMethodsSupported, &md.AuthorizationGrantProfilesSupported}
	for i, key := range metadataListKeys {
		if v, ok := r.strList(key, true); ok {
			*lists[i] = v
			md.present[key] = true
		}
	}
	md.ServiceDocumentation = r.url("service_documentation", false, true)
	md.OpPolicyURI = r.url("op_policy_uri", false, true)
	md.OpTosURI = r.url("op_tos_uri", false, true)
	md.RevocationEndpoint = r.url("revocation_endpoint", false, true)
	md.IntrospectionEndpoint = r.url("introspection_endpoint", false, true)
	md.ClientIDMetadataDocumentSupported = r.boolean("client_id_metadata_document_supported")
	md.AuthorizationResponseIssParameterSupported = r.boolean("authorization_response_iss_parameter_supported")
	if device {
		md.DeviceAuthorizationEndpoint = r.url("device_authorization_endpoint", true, true)
	}
	model := "OAuthMetadata"
	if device {
		model = "DeviceOAuthMetadata"
	}
	if err := r.err(model); err != nil {
		return nil, err
	}
	return md, nil
}

// Parses metadata JSON bytes into validated metadata
func parseOAuthMetadata(data []byte) (*OAuthMetadata, error) {
	doc, err := decodeJSONDocument(data)
	if err != nil {
		return nil, &ValidationError{Model: "OAuthMetadata", Errors: []FieldError{{"", "Invalid JSON: " + err.Error(), "json_invalid"}}}
	}
	m, ok := documentMap(doc)
	if !ok {
		return nil, &ValidationError{Model: "OAuthMetadata", Errors: []FieldError{{"", "Input should be an object", "model_type"}}}
	}
	return validateOAuthMetadata(m, false)
}

// Reports whether the metadata is the device authorization variant
func (md *OAuthMetadata) IsDevice() bool { return md.DeviceAuthorizationEndpoint != nil }

// Ordered exclude_none dump of the metadata
func (md *OAuthMetadata) dump() *jsonObject {
	d := newDump()
	d.o.set("issuer", md.Issuer)
	d.str("authorization_endpoint", md.AuthorizationEndpoint)
	d.o.set("token_endpoint", md.TokenEndpoint)
	d.str("registration_endpoint", md.RegistrationEndpoint).list("scopes_supported", md.ScopesSupported, md.HasScopesSupported).
		list("response_types_supported", md.ResponseTypesSupported, true)
	listVal := func(key string, v []string) { d.list(key, v, md.present[key]) }
	listVal("response_modes_supported", md.ResponseModesSupported)
	listVal("grant_types_supported", md.GrantTypesSupported)
	listVal("token_endpoint_auth_methods_supported", md.TokenEndpointAuthMethodsSupported)
	listVal("token_endpoint_auth_signing_alg_values_supported", md.TokenEndpointAuthSigningAlgValuesSupported)
	d.str("service_documentation", md.ServiceDocumentation)
	listVal("ui_locales_supported", md.UILocalesSupported)
	d.str("op_policy_uri", md.OpPolicyURI).str("op_tos_uri", md.OpTosURI).str("revocation_endpoint", md.RevocationEndpoint)
	listVal("revocation_endpoint_auth_methods_supported", md.RevocationEndpointAuthMethodsSupported)
	listVal("revocation_endpoint_auth_signing_alg_values_supported", md.RevocationEndpointAuthSigningAlgValuesSupported)
	d.str("introspection_endpoint", md.IntrospectionEndpoint)
	listVal("introspection_endpoint_auth_methods_supported", md.IntrospectionEndpointAuthMethodsSupported)
	listVal("introspection_endpoint_auth_signing_alg_values_supported", md.IntrospectionEndpointAuthSigningAlgValuesSupported)
	listVal("code_challenge_methods_supported", md.CodeChallengeMethodsSupported)
	d.boolean("client_id_metadata_document_supported", md.ClientIDMetadataDocumentSupported).
		boolean("authorization_response_iss_parameter_supported", md.AuthorizationResponseIssParameterSupported)
	listVal("authorization_grant_profiles_supported", md.AuthorizationGrantProfilesSupported)
	d.str("device_authorization_endpoint", md.DeviceAuthorizationEndpoint)
	return d.o
}

// ProtectedResourceMetadata is RFC 9728 protected resource metadata
type ProtectedResourceMetadata struct {
	Resource             string
	AuthorizationServers []string
	ScopesSupported      []string
	HasScopesSupported   bool
}

// Validates protected resource metadata, every declared field checked like the model
func validateProtectedResourceMetadata(m map[string]any) (*ProtectedResourceMetadata, error) {
	r := &modelReader{m: m}
	prm := &ProtectedResourceMetadata{}
	if s := r.url("resource", true, true); s != nil {
		prm.Resource = *s
	}
	if _, ok := m["authorization_servers"]; !ok {
		r.fail("authorization_servers", "Field required", "missing")
	} else if servers, present := r.urlList("authorization_servers", true, false); present {
		if len(servers) == 0 {
			r.fail("authorization_servers", "List should have at least 1 item after validation, not 0", "too_short")
		}
		prm.AuthorizationServers = servers
	}
	r.url("jwks_uri", false, true)
	prm.ScopesSupported, prm.HasScopesSupported = r.strList("scopes_supported", true)
	for _, key := range []string{"bearer_methods_supported", "resource_signing_alg_values_supported",
		"authorization_details_types_supported", "dpop_signing_alg_values_supported"} {
		r.strList(key, true)
	}
	r.str("resource_name", false)
	for _, key := range []string{"resource_documentation", "resource_policy_uri", "resource_tos_uri"} {
		r.url(key, false, true)
	}
	r.boolean("tls_client_certificate_bound_access_tokens")
	r.boolean("dpop_bound_access_tokens_required")
	if err := r.err("ProtectedResourceMetadata"); err != nil {
		return nil, err
	}
	return prm, nil
}

// Parses protected resource metadata JSON bytes
func parseProtectedResourceMetadata(data []byte) (*ProtectedResourceMetadata, error) {
	doc, err := decodeJSONDocument(data)
	if err != nil {
		return nil, &ValidationError{Model: "ProtectedResourceMetadata", Errors: []FieldError{{"", "Invalid JSON: " + err.Error(), "json_invalid"}}}
	}
	m, ok := documentMap(doc)
	if !ok {
		return nil, &ValidationError{Model: "ProtectedResourceMetadata", Errors: []FieldError{{"", "Input should be an object", "model_type"}}}
	}
	return validateProtectedResourceMetadata(m)
}
