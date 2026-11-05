package provider

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// One recorded call to the fake AWS endpoints
type awsRoleCall struct {
	Path, Signer, Token string
	Form                url.Values
	Header              http.Header
}

// Fake STS, IMDS, container, SSO portal and SSO OIDC endpoints recording every call
type awsRoleFake struct {
	srv      *httptest.Server
	mu       sync.Mutex
	calls    []awsRoleCall
	expiry   func(role string) string
	oidcBody string
}

var awsRoleSignerRe = regexp.MustCompile(`Credential=([^/]+)/`)

func newAWSRoleFake(t *testing.T) *awsRoleFake {
	f := &awsRoleFake{expiry: func(string) string { return "2099-01-01T00:00:00Z" }}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *awsRoleFake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	call := awsRoleCall{Path: r.URL.Path, Form: form, Header: r.Header.Clone(), Token: r.Header.Get("X-Amz-Security-Token")}
	if m := awsRoleSignerRe.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
		call.Signer = m[1]
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	switch {
	case r.URL.Path == "/" && form.Get("Action") == "AssumeRole":
		role := form.Get("RoleArn")[strings.LastIndex(form.Get("RoleArn"), "/")+1:]
		up := strings.ToUpper(role)
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials><AccessKeyId>AK%s</AccessKeyId><SecretAccessKey>S%s</SecretAccessKey><SessionToken>T%s</SessionToken><Expiration>%s</Expiration></Credentials></AssumeRoleResult></AssumeRoleResponse>`, up, up, up, f.expiry(role))
	case r.URL.Path == "/latest/api/token":
		_, _ = w.Write([]byte("imds-token"))
	case r.URL.Path == "/latest/meta-data/iam/security-credentials/":
		_, _ = w.Write([]byte("instance-role"))
	case r.URL.Path == "/latest/meta-data/iam/security-credentials/instance-role":
		_ = json.NewEncoder(w).Encode(map[string]string{"Code": "Success", "AccessKeyId": "AKIMDS", "SecretAccessKey": "SIMDS", "Token": "TIMDS", "Expiration": "2099-01-01T00:00:00Z"})
	case r.URL.Path == "/container":
		_ = json.NewEncoder(w).Encode(map[string]string{"AccessKeyId": "AKECS", "SecretAccessKey": "SECS", "Token": "TECS", "Expiration": "2099-01-01T00:00:00Z"})
	case r.URL.Path == "/token":
		_, _ = w.Write([]byte(f.oidcBody))
	case r.URL.Path == "/federation/credentials":
		bearer := r.Header.Get("x-amz-sso_bearer_token")
		if bearer == "expired" {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"roleCredentials": map[string]any{"accessKeyId": "AKSSO-" + bearer,
			"secretAccessKey": "SSSO", "sessionToken": "TSSO", "expiration": int64(4102444800000)}})
	default:
		w.WriteHeader(404)
	}
}

func (f *awsRoleFake) snapshot(path string) []awsRoleCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []awsRoleCall
	for _, c := range f.calls {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// Session over written config and credentials files with every AWS endpoint pointed at the fake
func awsRoleSession(t *testing.T, f *awsRoleFake, config, credentials string, env map[string]string) *awsSession {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	full := map[string]string{"AWS_ENDPOINT_URL_STS": f.srv.URL, "AWS_ENDPOINT_URL_SSO": f.srv.URL, "AWS_ENDPOINT_URL_SSO_OIDC": f.srv.URL,
		"AWS_REGION": "us-east-1", "HOME": dir}
	for k, v := range env {
		full[k] = v
	}
	s := awsTestSession(t, full)
	cfg, _ := s.lookup("AWS_CONFIG_FILE")
	creds, _ := s.lookup("AWS_SHARED_CREDENTIALS_FILE")
	awsWriteFile(t, cfg, config)
	awsWriteFile(t, creds, credentials)
	return s
}

func awsWantError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

func TestAWSChainAssumeRoleSourceProfileChain(t *testing.T) {
	f := newAWSRoleFake(t)
	s := awsRoleSession(t, f, `[profile mid]
role_arn = arn:aws:iam::111111111111:role/mid
source_profile = base
[profile top]
role_arn = arn:aws:iam::222222222222:role/top
source_profile = mid
external_id = ext-1
duration_seconds = 1800
role_session_name = sess-top
`, "[base]\naws_access_key_id = AKBASE\naws_secret_access_key = SBASE\n", map[string]string{"AWS_PROFILE": "top"})
	creds, err := awsResolve(t, s)
	if err != nil || creds.AccessKeyID != "AKTOP" || creds.SessionToken != "TTOP" || creds.Source != "assume-role" {
		t.Fatalf("%#v %v", creds, err)
	}
	calls := f.snapshot("/")
	if len(calls) != 2 {
		t.Fatalf("sts calls %d", len(calls))
	}
	first, second := calls[0], calls[1]
	if first.Form.Get("RoleArn") != "arn:aws:iam::111111111111:role/mid" || first.Signer != "AKBASE" || first.Token != "" {
		t.Fatalf("source role call %+v", first)
	}
	if second.Form.Get("RoleArn") != "arn:aws:iam::222222222222:role/top" || second.Signer != "AKMID" || second.Token != "TMID" ||
		second.Form.Get("ExternalId") != "ext-1" || second.Form.Get("DurationSeconds") != "1800" || second.Form.Get("RoleSessionName") != "sess-top" {
		t.Fatalf("top role call %+v", second)
	}
	if !strings.HasPrefix(first.Form.Get("RoleSessionName"), "botocore-session-") {
		t.Fatalf("default session name %q", first.Form.Get("RoleSessionName"))
	}
}

func TestAWSChainAssumeRoleConfigurationErrors(t *testing.T) {
	f := newAWSRoleFake(t)
	cases := []struct{ profile, config, credentials, want string }{
		{"a", "[profile a]\nrole_arn = arn:aws:iam::1:role/a\nsource_profile = b\n[profile b]\nrole_arn = arn:aws:iam::1:role/b\nsource_profile = a\n", "",
			"Infinite loop in credential configuration detected"},
		{"a", "[profile a]\nrole_arn = arn:aws:iam::1:role/a\nsource_profile = a\n", "", "Infinite loop in credential configuration detected"},
		{"a", "[profile a]\nrole_arn = arn:aws:iam::1:role/a\nsource_profile = ghost\n", "", `The source_profile "ghost" referenced in the profile "a" does not exist.`},
		{"a", "[profile a]\nrole_arn = arn:aws:iam::1:role/a\nsource_profile = b\ncredential_source = Environment\n[profile b]\n", "", "contains both source_profile and credential_source"},
		{"a", "[profile a]\nrole_arn = arn:aws:iam::1:role/a\ncredential_source = Nowhere\n", "", `The credential_source "Nowhere" is not a valid option.`},
		{"a", "[profile a]\nrole_arn = arn:aws:iam::1:role/a\ncredential_source = Ec2InstanceMetadata\n", "", "No credentials found in credential_source referenced in profile a"},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			_, err := awsResolve(t, awsRoleSession(t, f, tc.config, tc.credentials, map[string]string{"AWS_PROFILE": tc.profile}))
			awsWantError(t, err, tc.want)
		})
	}
	if calls := f.snapshot("/"); len(calls) != 0 {
		t.Fatalf("misconfigured chains reached STS %d times", len(calls))
	}
	t.Run("self source with static keys", func(t *testing.T) {
		s := awsRoleSession(t, f, "[profile self]\nrole_arn = arn:aws:iam::1:role/self\nsource_profile = self\n",
			"[self]\naws_access_key_id = AKSELF\naws_secret_access_key = SSELF\n", map[string]string{"AWS_PROFILE": "self"})
		creds, err := awsResolve(t, s)
		calls := f.snapshot("/")
		if err != nil || creds.AccessKeyID != "AKSELF" || len(calls) != 1 || calls[0].Signer != "AKSELF" {
			t.Fatalf("%#v %v %+v", creds, err, calls)
		}
	})
}

func TestAWSChainAssumeRoleCredentialSource(t *testing.T) {
	cases := []struct {
		source, signer, token string
		env                   func(f *awsRoleFake) map[string]string
	}{
		{"Environment", "AKENV", "TENV", func(*awsRoleFake) map[string]string {
			return map[string]string{"AWS_ACCESS_KEY_ID": "AKENV", "AWS_SECRET_ACCESS_KEY": "SENV", "AWS_SESSION_TOKEN": "TENV"}
		}},
		{"Ec2InstanceMetadata", "AKIMDS", "TIMDS", func(f *awsRoleFake) map[string]string {
			return map[string]string{"AWS_EC2_METADATA_DISABLED": "false", "AWS_EC2_METADATA_SERVICE_ENDPOINT": f.srv.URL}
		}},
		{"EcsContainer", "AKECS", "TECS", func(f *awsRoleFake) map[string]string {
			return map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": f.srv.URL + "/container"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			f := newAWSRoleFake(t)
			env := tc.env(f)
			s := awsRoleSession(t, f, "[profile worker]\nrole_arn = arn:aws:iam::1:role/worker\ncredential_source = "+tc.source+"\n", "", env)
			s.opts.Profile = "worker"
			creds, err := awsResolve(t, s)
			calls := f.snapshot("/")
			if err != nil || creds.AccessKeyID != "AKWORKER" || len(calls) != 1 || calls[0].Signer != tc.signer || calls[0].Token != tc.token {
				t.Fatalf("%#v %v %+v", creds, err, calls)
			}
		})
	}
}

func TestAWSChainAssumeRoleMFAPromptsOnceThenRefusesRefresh(t *testing.T) {
	f := newAWSRoleFake(t)
	f.expiry = func(string) string { return awsNow().Add(5 * time.Minute).UTC().Format(time.RFC3339) }
	prompts := 0
	prev := awsMFAPrompt
	awsMFAPrompt = func(serial string) (string, error) {
		prompts++
		if serial != "arn:aws:iam::1:mfa/me" {
			t.Errorf("serial %q", serial)
		}
		return "123456", nil
	}
	t.Cleanup(func() { awsMFAPrompt = prev })
	s := awsRoleSession(t, f, "[profile mfa]\nrole_arn = arn:aws:iam::1:role/mfa\nsource_profile = base\nmfa_serial = arn:aws:iam::1:mfa/me\n",
		"[base]\naws_access_key_id = AKBASE\naws_secret_access_key = SBASE\n", map[string]string{"AWS_PROFILE": "mfa"})
	src, err := resolveAWSCredentialSource(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := src.Retrieve(t.Context())
	calls := f.snapshot("/")
	if err != nil || creds.AccessKeyID != "AKMFA" || len(calls) != 1 || calls[0].Form.Get("SerialNumber") != "arn:aws:iam::1:mfa/me" || calls[0].Form.Get("TokenCode") != "123456" {
		t.Fatalf("%#v %v %+v", creds, err, calls)
	}
	_, err = src.Retrieve(t.Context())
	awsWantError(t, err, "Cannot refresh credentials: MFA token required.")
	if prompts != 1 || len(f.snapshot("/")) != 1 {
		t.Fatalf("prompts %d sts calls %d", prompts, len(f.snapshot("/")))
	}
}

// Writes an SSO token cache file the way the AWS CLI does, keyed by the sha1 of the start URL or session name
func awsWriteSSOCache(t *testing.T, key string, token map[string]any) string {
	sum := sha1.Sum([]byte(key))
	path := filepath.Join(os.Getenv("HOME"), ".aws", "sso", "cache", hex.EncodeToString(sum[:])+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(token)
	awsWriteFile(t, path, string(data))
	return path
}

const awsSSOLegacyConfig = "[profile sso]\nsso_start_url = https://start.example/start\nsso_region = us-east-1\nsso_account_id = 111111111111\nsso_role_name = Admin\n"

func TestAWSChainSSOLegacy(t *testing.T) {
	f := newAWSRoleFake(t)
	s := awsRoleSession(t, f, awsSSOLegacyConfig, "", map[string]string{"AWS_PROFILE": "sso"})
	_, err := awsResolve(t, s)
	awsWantError(t, err, "Token for https://start.example/start does not exist")
	awsWriteSSOCache(t, "https://start.example/start", map[string]any{"accessToken": "legacy-token", "expiresAt": "2099-01-01T00:00:00Z"})
	creds, err := awsResolve(t, s)
	calls := f.snapshot("/federation/credentials")
	if err != nil || creds.AccessKeyID != "AKSSO-legacy-token" || len(calls) == 0 {
		t.Fatalf("%#v %v", creds, err)
	}
	last := calls[len(calls)-1]
	if last.Header.Get("x-amz-sso_bearer_token") != "legacy-token" {
		t.Fatalf("%+v", last)
	}
	s = awsRoleSession(t, f, awsSSOLegacyConfig, "", map[string]string{"AWS_PROFILE": "sso"})
	awsWriteSSOCache(t, "https://start.example/start", map[string]any{"accessToken": "expired", "expiresAt": "2099-01-01T00:00:00Z"})
	_, err = awsResolve(t, s)
	awsWantError(t, err, "The SSO session associated with this profile has expired or is otherwise invalid")
	_, err = awsResolve(t, awsRoleSession(t, f, "[profile sso]\nsso_start_url = https://start.example/start\nsso_account_id = 1\nsso_role_name = Admin\n", "",
		map[string]string{"AWS_PROFILE": "sso"}))
	awsWantError(t, err, "is configured to use SSO but is missing required configuration: sso_region")
}

const awsSSOSessionConfig = "[profile ssos]\nsso_session = corp\nsso_account_id = 111111111111\nsso_role_name = Admin\n" +
	"[sso-session corp]\nsso_start_url = https://corp.example/start\nsso_region = us-east-1\nsso_registration_scopes = sso:account:access\n"

func TestAWSChainSSOSessionRefreshesAndWritesBack(t *testing.T) {
	f := newAWSRoleFake(t)
	f.oidcBody = `{"accessToken": "fresh-token", "expiresIn": 3600, "refreshToken": "refresh-2", "tokenType": "Bearer"}`
	s := awsRoleSession(t, f, awsSSOSessionConfig, "", map[string]string{"AWS_PROFILE": "ssos"})
	path := awsWriteSSOCache(t, "corp", map[string]any{"startUrl": "https://corp.example/start", "region": "us-east-1", "accessToken": "stale-token",
		"expiresAt": awsNow().Add(time.Minute).UTC().Format(time.RFC3339), "refreshToken": "refresh-1", "clientId": "client-1",
		"clientSecret": "secret-1", "registrationExpiresAt": "2099-01-01T00:00:00Z"})
	creds, err := awsResolve(t, s)
	if err != nil || creds.AccessKeyID != "AKSSO-fresh-token" {
		t.Fatalf("%#v %v", creds, err)
	}
	oidc := f.snapshot("/token")
	if len(oidc) != 1 {
		t.Fatalf("oidc calls %d", len(oidc))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cache map[string]any
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatal(err)
	}
	if cache["accessToken"] != "fresh-token" || cache["refreshToken"] != "refresh-2" || cache["clientId"] != "client-1" || cache["startUrl"] != "https://corp.example/start" {
		t.Fatalf("cache not written back %v", cache)
	}
	if exp, err := awsParseTime(cache["expiresAt"].(string)); err != nil || exp.Sub(awsNow()) < 50*time.Minute {
		t.Fatalf("expiresAt %v %v", cache["expiresAt"], err)
	}
	t.Run("expired token whose refresh fails", func(t *testing.T) {
		f.oidcBody = `{}`
		s := awsRoleSession(t, f, awsSSOSessionConfig, "", map[string]string{"AWS_PROFILE": "ssos"})
		awsWriteSSOCache(t, "corp", map[string]any{"accessToken": "dead", "expiresAt": "2000-01-01T00:00:00Z", "refreshToken": "r",
			"clientId": "c", "clientSecret": "s", "registrationExpiresAt": "2099-01-01T00:00:00Z"})
		_, err := awsResolve(t, s)
		awsWantError(t, err, "Token has expired and refresh failed")
	})
	t.Run("missing sso-session", func(t *testing.T) {
		_, err := awsResolve(t, awsRoleSession(t, f, "[profile ssos]\nsso_session = ghost\nsso_account_id = 1\nsso_role_name = Admin\n", "",
			map[string]string{"AWS_PROFILE": "ssos"}))
		awsWantError(t, err, `The specified sso-session does not exist: "ghost"`)
	})
	t.Run("inconsistent sso-session", func(t *testing.T) {
		_, err := awsResolve(t, awsRoleSession(t, f, strings.Replace(awsSSOSessionConfig, "sso_session = corp\n", "sso_session = corp\nsso_region = eu-west-1\n", 1), "",
			map[string]string{"AWS_PROFILE": "ssos"}))
		awsWantError(t, err, "The value for sso_region is inconsistent between profile (eu-west-1) and sso-session (us-east-1)")
	})
}
