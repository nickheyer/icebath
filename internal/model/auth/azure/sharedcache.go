package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	nameSharedCache        = "SharedTokenCacheCredential"
	sharedCacheName        = "msal.cache"
	keychainService        = "Microsoft.Developer.IdentityService"
	keychainAccount        = "MSALCache"
	sharedUnavailableText  = "Shared token cache unavailable"
	sharedNoAccounts       = "SharedTokenCacheCredential authentication unavailable. No accounts were found in the cache."
	sharedMultipleAccounts = "SharedTokenCacheCredential authentication unavailable. Multiple accounts\nwere found in the cache. Use username and tenant id to disambiguate."
	sharedMultipleMatching = "SharedTokenCacheCredential authentication unavailable. Multiple accounts\nmatching the specified"
	sharedNoMatching       = "SharedTokenCacheCredential authentication unavailable. No account\nmatching the specified"
	cacheLockWait          = 5 * time.Second
	cacheReadAttempts      = 3
	cacheReadPause         = 500 * time.Millisecond
	idTokenSkew            = 120
	clockAdvice            = "Make sure your computer's time and time zone are both correct."
	kindAccessToken        = "AccessToken"
	kindRefreshToken       = "RefreshToken"
	kindIDToken            = "IdToken"
	kindAccount            = "Account"
	kindAppMetadata        = "AppMetadata"
)

type cacheStore interface {
	location() string
	modified() (time.Time, bool, error)
	load(ctx context.Context) ([]byte, bool, error)
	save(ctx context.Context, data []byte) error
}

func fileModified(path string) (time.Time, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return info.ModTime(), true, nil
}

func writeInPlace(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func touchFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	now := time.Now()
	return os.Chtimes(path, now, now)
}

func readPlain(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

type jsonObject struct {
	keys   []string
	values map[string]json.RawMessage
}

func newObject() *jsonObject { return &jsonObject{values: map[string]json.RawMessage{}} }

func parseObject(data []byte) (*jsonObject, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	o := newObject()
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		o.set(key, raw)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return o, true
}

func (o *jsonObject) set(key string, value json.RawMessage) {
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

func (o *jsonObject) put(key string, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		raw = json.RawMessage("null")
	}
	o.set(key, raw)
}

func (o *jsonObject) remove(key string) {
	if _, ok := o.values[key]; !ok {
		return
	}
	delete(o.values, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *jsonObject) has(key string) bool {
	_, ok := o.values[key]
	return ok
}

func (o *jsonObject) text(key string) (string, bool) {
	raw, ok := o.values[key]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func (o *jsonObject) integer(key string) (int64, error) {
	raw, ok := o.values[key]
	if !ok {
		return 0, errors.New("the cache entry has no " + key)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, errors.New("the cache entry " + key + " is not a number")
	}
	return int64(n), nil
}

func (o *jsonObject) same(other *jsonObject, key string) bool {
	a, okA := o.values[key]
	b, okB := other.values[key]
	if !okA || !okB {
		return false
	}
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

func (o *jsonObject) clone() *jsonObject {
	c := newObject()
	for _, k := range o.keys {
		c.set(k, o.values[k])
	}
	return c
}

func (o *jsonObject) raw() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(k)
		b.Write(name)
		b.WriteByte(':')
		b.Write(o.values[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

type cacheEntry struct {
	key    string
	fields *jsonObject
}

type msalCache struct{ root *jsonObject }

func parseCache(data []byte) (*msalCache, error) {
	if len(data) == 0 {
		return &msalCache{root: newObject()}, nil
	}
	root, ok := parseObject(data)
	if !ok {
		return nil, errors.New("the token cache is not a JSON object")
	}
	return &msalCache{root: root}, nil
}

func (m *msalCache) section(kind string) *jsonObject {
	raw, ok := m.root.values[kind]
	if !ok {
		return nil
	}
	section, ok := parseObject(raw)
	if !ok {
		return nil
	}
	return section
}

func (m *msalCache) entries(kind string) []cacheEntry {
	section := m.section(kind)
	if section == nil {
		return nil
	}
	var out []cacheEntry
	for _, key := range section.keys {
		if fields, ok := parseObject(section.values[key]); ok {
			out = append(out, cacheEntry{key: key, fields: fields})
		}
	}
	return out
}

func (m *msalCache) store(kind, key string, fields *jsonObject) {
	section := m.section(kind)
	if section == nil {
		section = newObject()
	}
	if fields == nil {
		section.remove(key)
	} else {
		section.set(key, fields.raw())
	}
	m.root.set(kind, section.raw())
}

func (m *msalCache) encode() ([]byte, error) {
	return fileutil.EncodeJSON(m.root.raw(), fileutil.JSONStyle{Lines: true, Indent: 4, ASCII: true})
}

func keyPart(e *jsonObject, name string) string {
	s, _ := e.text(name)
	return s
}

func entryKey(kind string, e *jsonObject) string {
	p := func(name string) string { return keyPart(e, name) }
	switch kind {
	case kindAccessToken:
		return strings.ToLower(strings.Join([]string{p("home_account_id"), p("environment"), kindAccessToken, p("client_id"), p("realm"), p("target")}, "-"))
	case kindRefreshToken:
		return strings.ToLower(strings.Join([]string{p("home_account_id"), p("environment"), kindRefreshToken, p("client_id"), "", p("target")}, "-"))
	case kindIDToken:
		return strings.ToLower(strings.Join([]string{p("home_account_id"), p("environment"), kindIDToken, p("client_id"), p("realm"), ""}, "-"))
	case kindAccount:
		return strings.ToLower(strings.Join([]string{p("home_account_id"), p("environment"), p("realm")}, "-"))
	}
	return "appmetadata-" + p("environment") + "-" + p("client_id")
}

func (m *msalCache) modify(kind string, old *jsonObject, changes *jsonObject) error {
	if credential, _ := old.text("credential_type"); changes == nil && kind != kindAccount && kind != kindAppMetadata && credential != kind {
		return errors.New("the cache entry to remove is not a " + kind)
	}
	key := entryKey(kind, old)
	if changes == nil {
		m.store(kind, key, nil)
		return nil
	}
	merged := old.clone()
	for _, k := range changes.keys {
		merged.set(k, changes.values[k])
	}
	m.store(kind, key, merged)
	return nil
}

var knownAliases = [][]string{
	{"login.microsoftonline.com", "login.windows.net", "login.microsoft.com", "sts.windows.net"},
	{"login.chinacloudapi.cn", "login.partner.microsoftonline.cn"},
	{"login.microsoftonline.us", "login.usgovcloudapi.net"},
}

func authorityAliases(authority string) map[string]bool {
	host := authority
	if u, err := url.Parse(authority); err == nil {
		host = u.Host
	}
	for _, group := range knownAliases {
		if group[0] == host {
			aliases := map[string]bool{}
			for _, name := range group {
				aliases[name] = true
			}
			return aliases
		}
	}
	return map[string]bool{host: true}
}

type sharedCache struct {
	c         *client
	authority string
	aliases   map[string]bool
	username  string
	tenant    string

	mu     sync.Mutex
	store  cacheStore
	cache  *msalCache
	synced time.Time
}

func (s *sharedCache) name() string { return nameSharedCache }

func (s *sharedCache) String() string { return nameSharedCache + " at " + s.authority }

func (s *sharedCache) GoString() string { return s.String() }

func (s *sharedCache) LogValue() slog.Value { return slog.StringValue(s.String()) }

func (c *client) sharedTokenCache(username, tenant, authority string) source {
	return &sharedCache{c: c, authority: authority, aliases: authorityAliases(authority), username: username, tenant: tenant,
		cache: &msalCache{root: newObject()}}
}

func (s *sharedCache) reload(ctx context.Context) error {
	at, ok, err := s.store.modified()
	if err != nil {
		return err
	}
	if !ok || !at.After(s.synced) {
		return nil
	}
	data, found, err := s.store.load(ctx)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	parsed, err := parseCache(data)
	if err != nil {
		return err
	}
	s.cache, s.synced = parsed, time.Now()
	return nil
}

func (s *sharedCache) fresh(ctx context.Context) error {
	var err error
	for attempt := 1; attempt <= cacheReadAttempts; attempt++ {
		if err = s.reload(ctx); err == nil {
			return nil
		}
		if attempt < cacheReadAttempts {
			slog.Debug("Unable to load token cache file in No. " + strconv.Itoa(attempt) + " attempt")
			if waitErr := pause(ctx, cacheReadPause); waitErr != nil {
				return waitErr
			}
		}
	}
	return err
}

func lockCache(ctx context.Context, path string) (func() error, error) {
	wait, cancel := context.WithTimeout(ctx, cacheLockWait)
	defer cancel()
	lock, err := fileutil.OpenLock(wait, path)
	if err != nil {
		return nil, err
	}
	release := func() error {
		if err := lock.Release(); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission) {
			return err
		}
		return nil
	}
	f := lock.File()
	if err := f.Truncate(0); err != nil {
		_ = release()
		return nil, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+" "+os.Args[0]), 0); err != nil {
		_ = release()
		return nil, err
	}
	return release, nil
}

func (s *sharedCache) modify(ctx context.Context, change func(m *msalCache) error) error {
	release, err := lockCache(ctx, s.store.location()+".lockfile")
	if err != nil {
		return err
	}
	err = s.reload(ctx)
	if err == nil {
		err = change(s.cache)
	}
	var data []byte
	if err == nil {
		data, err = s.cache.encode()
	}
	if err == nil {
		err = s.store.save(ctx, data)
	}
	if err == nil {
		s.synced = time.Now()
	}
	if releaseErr := release(); err == nil {
		err = releaseErr
	}
	return err
}

func (s *sharedCache) token(ctx context.Context, scope string, _ bool) (accessToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil {
		store, err := openCacheStore(ctx)
		if err != nil {
			slog.Debug("Shared token cache could not be opened", "error", err)
			return accessToken{}, &UnavailableError{Credential: nameSharedCache, Message: sharedUnavailableText, Err: err}
		}
		s.store = store
	}
	if err := s.fresh(ctx); err != nil {
		return accessToken{}, &AuthError{Credential: nameSharedCache, Message: err.Error(), Err: err}
	}
	account, err := s.selectAccount()
	if err != nil {
		return accessToken{}, err
	}
	token, ok, err := s.cachedToken(ctx, scope, account)
	if err != nil {
		return accessToken{}, &UnavailableError{Credential: nameSharedCache, Message: "Error accessing cached data: " + err.Error(), Err: err}
	}
	if ok {
		return token, nil
	}
	secrets, err := s.refreshTokens(ctx, account)
	if err != nil {
		return accessToken{}, &UnavailableError{Credential: nameSharedCache, Message: "Error accessing cached refresh token: " + err.Error(), Err: err}
	}
	if len(secrets) == 0 {
		username, _ := account.text("username")
		return accessToken{}, unavailable(nameSharedCache, "Token acquisition failed for user '"+username+"'. To fix, re-authenticate\nthrough developer tooling supporting Azure single sign on")
	}
	return s.redeem(ctx, scope, secrets[0])
}

func (s *sharedCache) aliased(kind string) []*jsonObject {
	var out []*jsonObject
	for _, e := range s.cache.entries(kind) {
		if environment, ok := e.fields.text("environment"); ok && s.aliases[environment] {
			out = append(out, e.fields)
		}
	}
	return out
}

func (s *sharedCache) selectAccount() (*jsonObject, error) {
	accounts := s.aliased(kindAccount)
	var order []string
	byHome := map[string]*jsonObject{}
	for _, rt := range s.aliased(kindRefreshToken) {
		home, _ := rt.text("home_account_id")
		if home == "" || !rt.has("family_id") {
			continue
		}
		for _, account := range accounts {
			if id, ok := account.text("home_account_id"); ok && id == home {
				if _, seen := byHome[home]; !seen {
					order = append(order, home)
				}
				byHome[home] = account
			}
		}
	}
	if len(order) == 0 {
		return nil, unavailable(nameSharedCache, sharedNoAccounts)
	}
	var filtered []*jsonObject
	for _, home := range order {
		account := byHome[home]
		if name, _ := account.text("username"); s.username != "" && s.username != name {
			continue
		}
		if realm, _ := account.text("realm"); s.tenant != "" && s.tenant != realm {
			continue
		}
		filtered = append(filtered, account)
	}
	if len(filtered) == 1 {
		return filtered[0], nil
	}
	if s.username == "" && s.tenant == "" {
		return nil, unavailable(nameSharedCache, sharedMultipleAccounts)
	}
	specified := ""
	if s.username != "" {
		specified += " username: " + s.username
	}
	if s.tenant != "" {
		specified += " tenant: " + s.tenant
	}
	if len(filtered) > 1 {
		return nil, unavailable(nameSharedCache, sharedMultipleMatching+specified+" were found in the cache.")
	}
	return nil, unavailable(nameSharedCache, sharedNoMatching+specified+" was found in the cache.")
}

func targetCovers(entry *jsonObject, scope string) bool {
	target, _ := entry.text("target")
	for _, granted := range strings.Fields(target) {
		if granted == scope {
			return true
		}
	}
	return false
}

func (s *sharedCache) cachedToken(ctx context.Context, scope string, account *jsonObject) (accessToken, bool, error) {
	if !account.has("home_account_id") {
		return accessToken{}, false, nil
	}
	if err := s.fresh(ctx); err != nil {
		return accessToken{}, false, err
	}
	now := time.Now().Unix()
	var expired []*jsonObject
	for _, e := range s.cache.entries(kindAccessToken) {
		expiresOn, err := e.fields.integer("expires_on")
		if err != nil {
			return accessToken{}, false, err
		}
		if expiresOn < now {
			expired = append(expired, e.fields)
			continue
		}
		if !e.fields.same(account, "home_account_id") || !targetCovers(e.fields, scope) {
			continue
		}
		if expiresOn-refreshOffset > now {
			secret, _ := e.fields.text("secret")
			token := accessToken{value: secret, expiresOn: expiresOn}
			if e.fields.has("refresh_on") {
				if token.refreshOn, err = e.fields.integer("refresh_on"); err != nil {
					return accessToken{}, false, err
				}
			}
			return token, true, nil
		}
	}
	for _, stale := range expired {
		if err := s.modify(ctx, func(m *msalCache) error { return m.modify(kindAccessToken, stale, nil) }); err != nil {
			return accessToken{}, false, err
		}
	}
	return accessToken{}, false, nil
}

func (s *sharedCache) refreshTokens(ctx context.Context, account *jsonObject) ([]string, error) {
	if !account.has("home_account_id") {
		return nil, nil
	}
	if err := s.fresh(ctx); err != nil {
		return nil, err
	}
	var secrets []string
	for _, e := range s.cache.entries(kindRefreshToken) {
		if !e.fields.same(account, "home_account_id") || !e.fields.has("secret") {
			continue
		}
		secret, _ := e.fields.text("secret")
		secrets = append(secrets, secret)
	}
	return secrets, nil
}

func (s *sharedCache) holding(secret string) []*jsonObject {
	var out []*jsonObject
	for _, e := range s.cache.entries(kindRefreshToken) {
		if value, ok := e.fields.text("secret"); ok && value == secret {
			out = append(out, e.fields)
		}
	}
	return out
}

func (s *sharedCache) fail(status int, message string, err error) error {
	return &AuthError{Credential: nameSharedCache, Status: status, Message: message, Err: err}
}

func (s *sharedCache) redeem(ctx context.Context, scope, refreshToken string) (accessToken, error) {
	endpoint := s.authority + "/organizations/oauth2/v2.0/token"
	form := []pair{{"grant_type", "refresh_token"}, {"refresh_token", refreshToken}, {"scope", scope}, {"client_info", "1"}, {"client_id", developerClientID}}
	requestTime := time.Now().Unix()
	status, header, data, err := s.c.call(ctx, http.MethodPost, endpoint, http.Header{"Content-Type": {formType}}, encodeForm(form))
	if err != nil {
		return accessToken{}, s.fail(0, httpclient.Message(err), err)
	}
	reply, ok := parseDocument(data)
	if !ok {
		contentType := header.Get("Content-Type")
		if strings.HasPrefix(contentType, jsonType) {
			return accessToken{}, s.fail(status, "Failed to deserialize JSON from response", nil)
		}
		return accessToken{}, s.fail(status, `Unexpected content type "`+contentType+`"`, nil)
	}
	if reply.text("error") == "invalid_grant" {
		if err := s.fresh(ctx); err != nil {
			return accessToken{}, s.fail(status, err.Error(), err)
		}
		for _, stale := range s.holding(refreshToken) {
			if err := s.modify(ctx, func(m *msalCache) error { return m.modify(kindRefreshToken, stale, nil) }); err != nil {
				return accessToken{}, s.fail(status, err.Error(), err)
			}
		}
	}
	if _, rotated := reply["refresh_token"]; rotated {
		if err := s.fresh(ctx); err != nil {
			return accessToken{}, s.fail(status, err.Error(), err)
		}
		if held := s.holding(refreshToken); len(held) == 1 {
			changes := newObject()
			changes.put("secret", reply.text("refresh_token"))
			changes.put("last_modification_time", strconv.FormatInt(time.Now().Unix(), 10))
			if err := s.modify(ctx, func(m *msalCache) error { return m.modify(kindRefreshToken, held[0], changes) }); err != nil {
				return accessToken{}, s.fail(status, err.Error(), err)
			}
			delete(reply, "refresh_token")
		}
	}
	if _, failed := reply["error"]; failed {
		code := reply.text("error")
		if _, described := reply["error_description"]; described {
			return accessToken{}, &AuthError{Credential: nameSharedCache, Status: status, Code: code,
				Message: "Microsoft Entra ID error '(" + code + ") " + reply.text("error_description") + "'"}
		}
		return accessToken{}, &AuthError{Credential: nameSharedCache, Status: status, Code: code, Message: "Microsoft Entra ID error '" + code + "'"}
	}
	value := reply.text("access_token")
	token := accessToken{value: value}
	if on, ok := reply.integer("expires_on"); ok {
		token.expiresOn = on
	} else if lifetime, ok := reply.integer("expires_in"); ok {
		token.expiresOn = requestTime + lifetime
	} else {
		return accessToken{}, s.fail(status, unexpectedEntraReply, nil)
	}
	if value == "" {
		return accessToken{}, s.fail(status, unexpectedEntraReply, nil)
	}
	if in, ok := reply.integer("refresh_in"); ok {
		token.refreshOn = requestTime + in
	}
	claims, err := validatedClaims(reply, developerClientID, requestTime)
	if err != nil {
		return accessToken{}, s.fail(status, err.Error(), err)
	}
	if err := s.modify(ctx, func(m *msalCache) error {
		return m.add(reply, []string{scope}, endpoint, developerClientID, requestTime, claims)
	}); err != nil {
		return accessToken{}, s.fail(status, err.Error(), err)
	}
	return token, nil
}

func validatedClaims(reply document, clientID string, now int64) (document, error) {
	idToken := reply.text("id_token")
	if idToken == "" {
		return document{}, nil
	}
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return nil, errors.New("the identity token is not a JWT")
	}
	payload, err := decodeSegment(parts[1])
	if err != nil {
		return nil, err
	}
	claims, ok := parseDocument(payload)
	if !ok {
		return nil, errors.New("the identity token claims are not a JSON object")
	}
	reason := ""
	if nbf, ok := claims.integer("nbf"); ok && now+idTokenSkew < nbf {
		reason = "0. The ID token is not yet valid. " + clockAdvice
	}
	var audiences []string
	if json.Unmarshal(claims["aud"], &audiences) != nil {
		audiences = []string{claims.text("aud")}
	}
	if reason == "" && !contains(audiences, clientID) {
		reason = `3. The aud (audience) claim must contain this client's client_id "` + clientID + `", case-sensitively. Was your client_id in wrong casing?`
	}
	if exp, ok := claims.integer("exp"); reason == "" && (!ok || now-idTokenSkew > exp) {
		reason = "9. The ID token already expires. " + clockAdvice
	}
	if reason == "" {
		return claims, nil
	}
	shown, err := fileutil.EncodeJSON(json.RawMessage(payload), fileutil.JSONStyle{Lines: true, Indent: 2, ASCII: true})
	if err != nil {
		shown = payload
	}
	return nil, errors.New(reason + " Current epoch = " + strconv.FormatInt(now, 10) + ".  The id_token was: " + string(shown))
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func canonicalEndpoint(endpoint string) (string, string) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", ""
	}
	parts := strings.Split(u.Path, "/")
	realm := ""
	if len(parts) > 1 {
		realm = parts[1]
	}
	return strings.ToLower(u.Hostname()), realm
}

func (m *msalCache) add(reply document, scopes []string, endpoint, clientID string, now int64, claims document) error {
	environment, realm := canonicalEndpoint(endpoint)
	var home any
	hasAccount := false
	if _, present := reply["client_info"]; present {
		raw, err := decodeSegment(reply.text("client_info"))
		if err != nil {
			return err
		}
		info, ok := parseDocument(raw)
		if !ok {
			return errors.New("client_info is not a JSON object")
		}
		if _, uid := info["uid"]; uid {
			if _, utid := info["utid"]; utid {
				home, hasAccount = info.text("uid")+"."+info.text("utid"), true
			}
		}
	}
	if !hasAccount && len(claims) > 0 {
		if _, present := claims["sub"]; !present {
			return errors.New("the identity token carries no subject")
		}
		home, hasAccount = claims.text("sub"), true
	}
	sorted := append([]string(nil), scopes...)
	sort.Strings(sorted)
	target := strings.Join(sorted, " ")
	stamp := func(seconds int64) string { return strconv.FormatInt(seconds, 10) }
	if reply.text("access_token") != "" {
		lifetime := int64(600)
		if on, ok := reply.integer("expires_on"); ok {
			lifetime = on - now
		}
		if in, ok := reply.integer("expires_in"); ok {
			lifetime = in
		}
		extended := lifetime
		if ext, ok := reply.integer("ext_expires_in"); ok {
			extended = ext
		}
		at := newObject()
		at.put("credential_type", kindAccessToken)
		at.put("secret", reply.text("access_token"))
		at.put("home_account_id", home)
		at.put("environment", environment)
		at.put("client_id", clientID)
		at.put("target", target)
		at.put("realm", realm)
		if raw, ok := reply["token_type"]; ok {
			at.set("token_type", raw)
		} else {
			at.put("token_type", "Bearer")
		}
		at.put("cached_at", stamp(now))
		at.put("expires_on", stamp(now+lifetime))
		at.put("extended_expires_on", stamp(now+extended))
		if in, ok := reply.integer("refresh_in"); ok {
			at.put("refresh_on", stamp(now+in))
		}
		if err := m.modify(kindAccessToken, at, at); err != nil {
			return err
		}
	}
	if hasAccount {
		account := newObject()
		account.put("home_account_id", home)
		account.put("environment", environment)
		account.put("realm", realm)
		if raw, ok := claims["oid"]; ok {
			account.set("local_account_id", raw)
		} else if raw, ok := claims["sub"]; ok {
			account.set("local_account_id", raw)
		} else {
			account.put("local_account_id", nil)
		}
		username := claims.text("preferred_username")
		if username == "" {
			username = claims.text("upn")
		}
		account.put("username", username)
		authorityType := "MSSTS"
		if realm == "adfs" {
			authorityType = "ADFS"
		}
		account.put("authority_type", authorityType)
		if err := m.modify(kindAccount, account, account); err != nil {
			return err
		}
	}
	if idToken := reply.text("id_token"); idToken != "" {
		idt := newObject()
		idt.put("credential_type", kindIDToken)
		idt.put("secret", idToken)
		idt.put("home_account_id", home)
		idt.put("environment", environment)
		idt.put("realm", realm)
		idt.put("client_id", clientID)
		if err := m.modify(kindIDToken, idt, idt); err != nil {
			return err
		}
	}
	foci, family := reply["foci"]
	if next := reply.text("refresh_token"); next != "" {
		rt := newObject()
		rt.put("credential_type", kindRefreshToken)
		rt.put("secret", next)
		rt.put("home_account_id", home)
		rt.put("environment", environment)
		rt.put("client_id", clientID)
		rt.put("target", target)
		rt.put("last_modification_time", stamp(now))
		if family {
			rt.set("family_id", foci)
		}
		if err := m.modify(kindRefreshToken, rt, rt); err != nil {
			return err
		}
	}
	metadata := newObject()
	metadata.put("client_id", clientID)
	metadata.put("environment", environment)
	if family {
		metadata.set("family_id", foci)
	}
	return m.modify(kindAppMetadata, metadata, metadata)
}
