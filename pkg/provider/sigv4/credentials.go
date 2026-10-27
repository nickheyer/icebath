package sigv4

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/shell"
)

// Credentials from a named profile in the shared config and credentials files
type Profile struct {
	// AWS_PROFILE or default when empty
	Name string
	// AWS_SHARED_CREDENTIALS_FILE or the .aws/credentials file in the home directory when empty
	File string
	// AWS_CONFIG_FILE or the .aws/config file in the home directory when empty
	Config string
	// Used for STS and SSO calls, a ten second client when nil
	Client *http.Client
	// Supplies the code for profiles naming mfa_serial, which fail without one
	TokenCode func(ctx context.Context, serial string) (string, error)
}

// Sections of both shared files, profiles merged with the credentials file winning
type shared struct {
	profiles map[string]map[string]string
	sessions map[string]map[string]string
	paths    string
}

func (p Profile) name() string {
	if p.Name != "" {
		return p.Name
	}
	if env := os.Getenv("AWS_PROFILE"); env != "" {
		return env
	}
	return "default"
}

func (p Profile) client() *http.Client { return httpClient(p.Client, 10*time.Second) }

// Picks the explicit path, then the environment, then the file under the home directory
func sharedPath(explicit, env, name string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if v := os.Getenv(env); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aws", name), nil
}

// Reads both shared files, tolerating one that is missing
func (p Profile) load() (shared, error) {
	credPath, err := sharedPath(p.File, "AWS_SHARED_CREDENTIALS_FILE", "credentials")
	if err != nil {
		return shared{}, fmt.Errorf("sigv4: %w", err)
	}
	confPath, err := sharedPath(p.Config, "AWS_CONFIG_FILE", "config")
	if err != nil {
		return shared{}, fmt.Errorf("sigv4: %w", err)
	}
	sh := shared{profiles: map[string]map[string]string{}, sessions: map[string]map[string]string{}, paths: credPath + " or " + confPath}
	found := 0
	for _, path := range []string{confPath, credPath} {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return shared{}, fmt.Errorf("sigv4: %w", err)
		}
		found++
		for name, section := range ini(string(data)) {
			target := sh.profiles
			if rest, ok := strings.CutPrefix(name, "sso-session "); ok {
				target, name = sh.sessions, strings.TrimSpace(rest)
			}
			merged := target[name]
			if merged == nil {
				merged = map[string]string{}
				target[name] = merged
			}
			for k, v := range section {
				merged[k] = v
			}
		}
	}
	if found == 0 {
		return shared{}, fmt.Errorf("sigv4: neither %s exists", sh.paths)
	}
	return sh, nil
}

// Parses INI sections into maps, dropping a leading profile word and skipping indented nested values
func ini(text string) map[string]map[string]string {
	out := map[string]map[string]string{}
	current := ""
	nested := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimPrefix(strings.TrimSpace(line[1:len(line)-1]), "profile ")
			if _, ok := out[current]; !ok {
				out[current] = map[string]string{}
			}
			nested = false
			continue
		}
		indented := raw != "" && (raw[0] == ' ' || raw[0] == '\t')
		if indented && nested {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || current == "" {
			continue
		}
		v = strings.TrimSpace(v)
		nested = v == ""
		if !nested {
			out[current][strings.ToLower(strings.TrimSpace(k))] = v
		}
	}
	return out
}

func (p Profile) Credentials(ctx context.Context) (Credentials, error) {
	sh, err := p.load()
	if err != nil {
		return Credentials{}, err
	}
	return p.resolve(ctx, sh, p.name(), map[string]bool{})
}

// Resolves one profile by the credential kind it declares, following source profiles without looping
func (p Profile) resolve(ctx context.Context, sh shared, name string, visited map[string]bool) (Credentials, error) {
	if visited[name] {
		return Credentials{}, fmt.Errorf("sigv4: profile %q refers back to itself through source_profile", name)
	}
	visited[name] = true
	section, ok := sh.profiles[name]
	if !ok {
		return Credentials{}, fmt.Errorf("sigv4: profile %q not found in %s", name, sh.paths)
	}
	static := Credentials{AccessKey: section["aws_access_key_id"], SecretKey: section["aws_secret_access_key"], SessionToken: section["aws_session_token"]}
	switch {
	case section["role_arn"] != "" && section["web_identity_token_file"] != "":
		token, err := os.ReadFile(section["web_identity_token_file"])
		if err != nil {
			return Credentials{}, fmt.Errorf("sigv4: profile %q: %w", name, err)
		}
		seconds, err := duration(name, section)
		if err != nil {
			return Credentials{}, err
		}
		return assumeRoleWithWebIdentity(ctx, p.client(), profileRegion(section), section["role_arn"], sessionName(section["role_session_name"]), strings.TrimSpace(string(token)), seconds)
	case section["role_arn"] != "":
		return p.assume(ctx, sh, name, section, static, visited)
	case section["sso_session"] != "" || section["sso_start_url"] != "":
		return p.sso(ctx, sh, name, section)
	case section["credential_process"] != "":
		return runProcess(ctx, section["credential_process"])
	case static.AccessKey != "" || static.SecretKey != "":
		if !static.valid() {
			return Credentials{}, fmt.Errorf("sigv4: profile %q in %s lacks a key pair", name, sh.paths)
		}
		return static, nil
	}
	return Credentials{}, fmt.Errorf("sigv4: profile %q in %s names no credentials", name, sh.paths)
}

// Assumes the role of a profile with credentials from its source profile, credential source or own keys
func (p Profile) assume(ctx context.Context, sh shared, name string, section map[string]string, static Credentials, visited map[string]bool) (Credentials, error) {
	var source Credentials
	var err error
	switch {
	case section["source_profile"] != "":
		source, err = p.resolve(ctx, sh, section["source_profile"], visited)
	case section["credential_source"] != "":
		source, err = p.credentialSource(ctx, section["credential_source"])
	case static.valid():
		source = static
	default:
		err = fmt.Errorf("sigv4: profile %q names role_arn without source_profile or credential_source", name)
	}
	if err != nil {
		return Credentials{}, err
	}
	seconds, err := duration(name, section)
	if err != nil {
		return Credentials{}, err
	}
	req := roleRequest{RoleARN: section["role_arn"], SessionName: sessionName(section["role_session_name"]), ExternalID: section["external_id"], Duration: seconds, Serial: section["mfa_serial"]}
	if req.Serial != "" {
		if p.TokenCode == nil {
			return Credentials{}, fmt.Errorf("sigv4: profile %q names mfa_serial and no TokenCode is set", name)
		}
		if req.Code, err = p.TokenCode(ctx, req.Serial); err != nil {
			return Credentials{}, fmt.Errorf("sigv4: profile %q: %w", name, err)
		}
	}
	return assumeRole(ctx, p.client(), profileRegion(section), req, source)
}

// Maps a credential_source value onto the matching source, the link local ones reached without the STS client
func (Profile) credentialSource(ctx context.Context, kind string) (Credentials, error) {
	switch kind {
	case "Environment":
		return Env{}.Credentials(ctx)
	case "Ec2InstanceMetadata":
		return IMDS{}.Credentials(ctx)
	case "EcsContainer":
		return Container{}.Credentials(ctx)
	}
	return Credentials{}, fmt.Errorf("sigv4: unknown credential_source %q", kind)
}

// Region a profile names, else the environment
func profileRegion(section map[string]string) string {
	if r := section["region"]; r != "" {
		return r
	}
	return Region()
}

func sessionName(configured string) string {
	if configured != "" {
		return configured
	}
	return "icebath-" + strconv.FormatInt(time.Now().Unix(), 10)
}

// Parses duration_seconds, zero when absent so STS applies its default
func duration(profile string, section map[string]string) (int, error) {
	raw := section["duration_seconds"]
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("sigv4: profile %q duration_seconds %q is not a positive number", profile, raw)
	}
	return n, nil
}

// Budget for a credential_process command
const ProcessTimeout = time.Minute

// Credential document a credential_process prints
type processOutput struct {
	Version         int
	AccessKeyId     string
	SecretAccessKey string
	SessionToken    string
	Expiration      string
}

// Runs a credential_process command and reads the version 1 JSON it prints
func runProcess(ctx context.Context, command string) (Credentials, error) {
	ctx, cancel := context.WithTimeout(ctx, ProcessTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code, err := shell.Cmd{Line: command, Stdout: &stdout, Stderr: &stderr}.Run(ctx)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit status %d", code)
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: credential_process: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var out processOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return Credentials{}, fmt.Errorf("sigv4: credential_process output: %w", err)
	}
	if out.Version != 1 {
		return Credentials{}, fmt.Errorf("sigv4: credential_process output version %d is not 1", out.Version)
	}
	c := Credentials{AccessKey: out.AccessKeyId, SecretKey: out.SecretAccessKey, SessionToken: out.SessionToken}
	if !c.valid() {
		return Credentials{}, errors.New("sigv4: credential_process output lacks a key pair")
	}
	if out.Expiration != "" {
		at, err := time.Parse(time.RFC3339, out.Expiration)
		if err != nil {
			return Credentials{}, fmt.Errorf("sigv4: credential_process expiration: %w", err)
		}
		c.Expires = at
	}
	return c, nil
}

// Token the AWS CLI caches after aws sso login, refreshed here when it holds a registration
type ssoCache struct {
	AccessToken           string  `json:"accessToken"`
	ExpiresAt             ssoTime `json:"expiresAt"`
	Region                string  `json:"region,omitempty"`
	StartURL              string  `json:"startUrl,omitempty"`
	RefreshToken          string  `json:"refreshToken,omitempty"`
	ClientID              string  `json:"clientId,omitempty"`
	ClientSecret          string  `json:"clientSecret,omitempty"`
	RegistrationExpiresAt ssoTime `json:"registrationExpiresAt,omitempty"`
}

// Time as the CLI writes it, RFC 3339 or the older form ending in UTC
type ssoTime struct{ time.Time }

func (t *ssoTime) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		t.Time = time.Time{}
		return nil
	}
	if rest, ok := strings.CutSuffix(s, "UTC"); ok {
		s = rest + "Z"
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

func (t ssoTime) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte(`""`), nil
	}
	return json.Marshal(t.UTC().Format(time.RFC3339))
}

// Resolves an SSO profile through the cached login token and the portal credentials call
func (p Profile) sso(ctx context.Context, sh shared, name string, section map[string]string) (Credentials, error) {
	startURL, region, key := section["sso_start_url"], section["sso_region"], section["sso_start_url"]
	if sess := section["sso_session"]; sess != "" {
		s, ok := sh.sessions[sess]
		if !ok {
			return Credentials{}, fmt.Errorf("sigv4: profile %q names sso-session %q, which %s lacks", name, sess, sh.paths)
		}
		startURL, region, key = s["sso_start_url"], s["sso_region"], sess
	}
	account, role := section["sso_account_id"], section["sso_role_name"]
	if startURL == "" || region == "" || account == "" || role == "" {
		return Credentials{}, fmt.Errorf("sigv4: profile %q needs sso_start_url, sso_region, sso_account_id and sso_role_name", name)
	}
	token, err := ssoToken(ctx, p.client(), key, region, name)
	if err != nil {
		return Credentials{}, err
	}
	return ssoCredentials(ctx, p.client(), region, token, account, role)
}

// Path of the cached token for a session name or start URL
func ssoCachePath(key string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	sum := sha1.Sum([]byte(key))
	return filepath.Join(home, ".aws", "sso", "cache", hex.EncodeToString(sum[:])+".json"), nil
}

// Loads the cached token, refreshing an expired one through OIDC when the cache holds a registration
func ssoToken(ctx context.Context, client *http.Client, key, region, profile string) (string, error) {
	path, err := ssoCachePath(key)
	if err != nil {
		return "", fmt.Errorf("sigv4: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("sigv4: profile %q has no sso login, run aws sso login --profile %s: %w", profile, profile, err)
	}
	var cache ssoCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return "", fmt.Errorf("sigv4: %s: %w", path, err)
	}
	if cache.AccessToken != "" && time.Now().Before(cache.ExpiresAt.Time) {
		return cache.AccessToken, nil
	}
	if cache.RefreshToken == "" || cache.ClientID == "" || cache.ClientSecret == "" || (!cache.RegistrationExpiresAt.IsZero() && time.Now().After(cache.RegistrationExpiresAt.Time)) {
		return "", fmt.Errorf("sigv4: sso login for profile %q expired, run aws sso login --profile %s", profile, profile)
	}
	if cache.Region == "" {
		cache.Region = region
	}
	if err := ssoRefresh(ctx, client, &cache); err != nil {
		return "", err
	}
	if err := writeFile(path, cache); err != nil {
		return "", fmt.Errorf("sigv4: %w", err)
	}
	return cache.AccessToken, nil
}

// Trades the refresh token for a new access token at the OIDC endpoint
func ssoRefresh(ctx context.Context, client *http.Client, cache *ssoCache) error {
	body, err := json.Marshal(map[string]string{"clientId": cache.ClientID, "clientSecret": cache.ClientSecret, "grantType": "refresh_token", "refreshToken": cache.RefreshToken})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oidc."+cache.Region+".amazonaws.com/token", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sigv4: sso oidc: %w", err)
	}
	var out struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int    `json:"expiresIn"`
	}
	if err := readJSON(resp, &out); err != nil {
		return fmt.Errorf("sigv4: sso oidc: %w", err)
	}
	if out.AccessToken == "" || out.ExpiresIn <= 0 {
		return errors.New("sigv4: sso oidc returned no access token")
	}
	cache.AccessToken = out.AccessToken
	cache.ExpiresAt = ssoTime{time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).UTC()}
	if out.RefreshToken != "" {
		cache.RefreshToken = out.RefreshToken
	}
	return nil
}

// Asks the SSO portal for the role credentials the token grants
func ssoCredentials(ctx context.Context, client *http.Client, region, token, account, role string) (Credentials, error) {
	q := url.Values{"account_id": {account}, "role_name": {role}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://portal.sso."+region+".amazonaws.com/federation/credentials?"+q.Encode(), nil)
	if err != nil {
		return Credentials{}, err
	}
	req.Header.Set("x-amz-sso_bearer_token", token)
	resp, err := client.Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: sso portal: %w", err)
	}
	var out struct {
		RoleCredentials struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
			Expiration      int64  `json:"expiration"`
		} `json:"roleCredentials"`
	}
	if err := readJSON(resp, &out); err != nil {
		return Credentials{}, fmt.Errorf("sigv4: sso portal: %w", err)
	}
	rc := out.RoleCredentials
	c := Credentials{AccessKey: rc.AccessKeyID, SecretKey: rc.SecretAccessKey, SessionToken: rc.SessionToken}
	if !c.valid() {
		return Credentials{}, errors.New("sigv4: sso portal returned no key pair")
	}
	if rc.Expiration > 0 {
		c.Expires = time.UnixMilli(rc.Expiration).UTC()
	}
	return c, nil
}

// Writes the cache document through a temporary file readable only by the owner
func writeFile(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

const stsVersion = "2011-06-15"

// Role a profile or the environment asks STS for
type roleRequest struct {
	RoleARN     string
	SessionName string
	ExternalID  string
	Duration    int
	Serial      string
	Code        string
}

// Credentials from AWS_WEB_IDENTITY_TOKEN_FILE and AWS_ROLE_ARN exchanged at STS
type WebIdentity struct {
	// Used for the STS call, a ten second client when nil
	Client *http.Client
}

func (w WebIdentity) Credentials(ctx context.Context) (Credentials, error) {
	file, role := os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE"), os.Getenv("AWS_ROLE_ARN")
	if file == "" || role == "" {
		return Credentials{}, errors.New("sigv4: AWS_WEB_IDENTITY_TOKEN_FILE and AWS_ROLE_ARN are not both set")
	}
	token, err := os.ReadFile(file)
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: %w", err)
	}
	return assumeRoleWithWebIdentity(ctx, httpClient(w.Client, 10*time.Second), Region(), role, sessionName(os.Getenv("AWS_ROLE_SESSION_NAME")), strings.TrimSpace(string(token)), 0)
}

// Regional STS endpoint and signing region, the global endpoint in us-east-1 when no region is known
func stsEndpoint(region string) (string, string) {
	if region == "" {
		return "https://sts.amazonaws.com/", "us-east-1"
	}
	return "https://sts." + region + ".amazonaws.com/", region
}

// Calls AssumeRole signed with the source credentials
func assumeRole(ctx context.Context, client *http.Client, region string, req roleRequest, source Credentials) (Credentials, error) {
	form := url.Values{"Action": {"AssumeRole"}, "Version": {stsVersion}, "RoleArn": {req.RoleARN}, "RoleSessionName": {req.SessionName}}
	if req.ExternalID != "" {
		form.Set("ExternalId", req.ExternalID)
	}
	if req.Duration > 0 {
		form.Set("DurationSeconds", strconv.Itoa(req.Duration))
	}
	if req.Serial != "" {
		form.Set("SerialNumber", req.Serial)
		form.Set("TokenCode", req.Code)
	}
	return stsCall(ctx, client, region, form, &source)
}

// Calls AssumeRoleWithWebIdentity, which STS accepts unsigned
func assumeRoleWithWebIdentity(ctx context.Context, client *http.Client, region, roleARN, session, token string, durationSeconds int) (Credentials, error) {
	form := url.Values{"Action": {"AssumeRoleWithWebIdentity"}, "Version": {stsVersion}, "RoleArn": {roleARN}, "RoleSessionName": {session}, "WebIdentityToken": {token}}
	if durationSeconds > 0 {
		form.Set("DurationSeconds", strconv.Itoa(durationSeconds))
	}
	return stsCall(ctx, client, region, form, nil)
}

// Credentials element shared by every STS assume role response
type stsCredentials struct {
	AccessKeyId     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

type stsError struct {
	Code    string `xml:"Error>Code"`
	Message string `xml:"Error>Message"`
}

// Posts a query API form to STS and reads the credentials out of whichever result element answers
func stsCall(ctx context.Context, client *http.Client, region string, form url.Values, sign *Credentials) (Credentials, error) {
	endpoint, signRegion := stsEndpoint(region)
	body := form.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return Credentials{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("Accept", "application/xml")
	if sign != nil {
		if err := Sign(req, *sign, signRegion, "sts", time.Now()); err != nil {
			return Credentials{}, err
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: sts: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: sts: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var failure stsError
		if xml.Unmarshal(data, &failure) == nil && failure.Code != "" {
			return Credentials{}, fmt.Errorf("sigv4: sts %s: %s", failure.Code, failure.Message)
		}
		return Credentials{}, fmt.Errorf("sigv4: sts http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	creds, err := stsResult(data)
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: sts %s: %w", form.Get("Action"), err)
	}
	return creds, nil
}

// Finds the Credentials element anywhere in an STS response and decodes it
func stsResult(data []byte) (Credentials, error) {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		tok, err := dec.Token()
		if err != nil {
			return Credentials{}, fmt.Errorf("response holds no credentials: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "Credentials" {
			continue
		}
		var c stsCredentials
		if err := dec.DecodeElement(&c, &start); err != nil {
			return Credentials{}, err
		}
		out := Credentials{AccessKey: c.AccessKeyId, SecretKey: c.SecretAccessKey, SessionToken: c.SessionToken, Expires: c.Expiration}
		if !out.valid() || out.SessionToken == "" {
			return Credentials{}, errors.New("response credentials are incomplete")
		}
		return out, nil
	}
}

// Credential document the container and instance metadata endpoints return
type remoteCredentials struct {
	Code            string
	AccessKeyId     string
	SecretAccessKey string
	Token           string
	Expiration      time.Time
}

func (r remoteCredentials) credentials() (Credentials, error) {
	c := Credentials{AccessKey: r.AccessKeyId, SecretKey: r.SecretAccessKey, SessionToken: r.Token, Expires: r.Expiration}
	if !c.valid() {
		return Credentials{}, errors.New("document lacks a key pair")
	}
	return c, nil
}

// Credentials from the endpoint AWS_CONTAINER_CREDENTIALS_RELATIVE_URI or AWS_CONTAINER_CREDENTIALS_FULL_URI names
type Container struct {
	// A five second client when nil
	Client *http.Client
}

func (c Container) Credentials(ctx context.Context) (Credentials, error) {
	target := os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI")
	if rel := os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"); target == "" && rel != "" {
		target = "http://169.254.170.2" + rel
	}
	if target == "" {
		return Credentials{}, errors.New("sigv4: AWS_CONTAINER_CREDENTIALS_RELATIVE_URI and AWS_CONTAINER_CREDENTIALS_FULL_URI are not set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Credentials{}, err
	}
	token := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN")
	if file := os.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return Credentials{}, fmt.Errorf("sigv4: container: %w", err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := httpClient(c.Client, 5*time.Second).Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: container: %w", err)
	}
	var doc remoteCredentials
	if err := readJSON(resp, &doc); err != nil {
		return Credentials{}, fmt.Errorf("sigv4: container: %w", err)
	}
	creds, err := doc.credentials()
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: container: %w", err)
	}
	return creds, nil
}

// Credentials of the instance role from the EC2 metadata service, version 2 with a session token
type IMDS struct {
	// A one second client when nil
	Client *http.Client
}

// Lifetime asked for the metadata session token
const imdsTokenTTL = "21600"

func (i IMDS) Credentials(ctx context.Context) (Credentials, error) {
	if strings.EqualFold(os.Getenv("AWS_EC2_METADATA_DISABLED"), "true") {
		return Credentials{}, errors.New("sigv4: instance metadata is disabled by AWS_EC2_METADATA_DISABLED")
	}
	base := strings.TrimRight(os.Getenv("AWS_EC2_METADATA_SERVICE_ENDPOINT"), "/")
	if base == "" {
		base = "http://169.254.169.254"
	}
	client := httpClient(i.Client, time.Second)
	token, err := i.text(ctx, client, http.MethodPut, base+"/latest/api/token", http.Header{"X-Aws-Ec2-Metadata-Token-Ttl-Seconds": {imdsTokenTTL}})
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: imds token: %w", err)
	}
	auth := http.Header{"X-Aws-Ec2-Metadata-Token": {token}}
	roles, err := i.text(ctx, client, http.MethodGet, base+"/latest/meta-data/iam/security-credentials/", auth)
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: imds role: %w", err)
	}
	role, _, _ := strings.Cut(strings.TrimSpace(roles), "\n")
	if role == "" {
		return Credentials{}, errors.New("sigv4: imds names no instance role")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/latest/meta-data/iam/security-credentials/"+role, nil)
	if err != nil {
		return Credentials{}, err
	}
	req.Header = auth
	resp, err := client.Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: imds: %w", err)
	}
	var doc remoteCredentials
	if err := readJSON(resp, &doc); err != nil {
		return Credentials{}, fmt.Errorf("sigv4: imds: %w", err)
	}
	if doc.Code != "" && doc.Code != "Success" {
		return Credentials{}, fmt.Errorf("sigv4: imds role %s: %s", role, doc.Code)
	}
	creds, err := doc.credentials()
	if err != nil {
		return Credentials{}, fmt.Errorf("sigv4: imds: %w", err)
	}
	return creds, nil
}

// Issues one metadata request and returns its body as text
func (IMDS) text(ctx context.Context, client *http.Client, method, target string, header http.Header) (string, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return "", err
	}
	req.Header = header
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return string(data), nil
}
