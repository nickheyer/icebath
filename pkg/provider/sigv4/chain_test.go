package sigv4

import (
	"context"
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
	"strings"
	"sync"
	"testing"
	"time"
)

// Transport steering every request at the test server while remembering the host it was meant for
type rewrite struct{ to *url.URL }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("X-Meant-For", req.URL.Host)
	req.URL.Scheme, req.URL.Host = r.to.Scheme, r.to.Host
	return http.DefaultTransport.RoundTrip(req)
}

func steer(t *testing.T, h http.Handler) (*http.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return &http.Client{Transport: rewrite{to: u}}, srv
}

func write(t *testing.T, path, content string) string {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const stsXML = `<%[1]sResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><%[1]sResult><Credentials><AccessKeyId>%[2]s</AccessKeyId><SecretAccessKey>%[3]s</SecretAccessKey><SessionToken>%[4]s</SessionToken><Expiration>2031-01-02T03:04:05Z</Expiration></Credentials></%[1]sResult></%[1]sResponse>`

// Fake STS recording every form it answers
type sts struct {
	mu    sync.Mutex
	forms []url.Values
	auths []string
	hosts []string
}

func (s *sts) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	s.mu.Lock()
	s.forms = append(s.forms, r.PostForm)
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	s.hosts = append(s.hosts, r.Header.Get("X-Meant-For"))
	s.mu.Unlock()
	action := r.PostForm.Get("Action")
	switch {
	case r.PostForm.Get("RoleArn") == "arn:aws:iam::1:role/denied":
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<ErrorResponse><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>not for you</Message></Error></ErrorResponse>`)
	case action == "AssumeRole" && !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential="):
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<ErrorResponse><Error><Code>MissingAuthenticationToken</Code><Message>unsigned</Message></Error></ErrorResponse>`)
	case action == "AssumeRoleWithWebIdentity" && r.PostForm.Get("WebIdentityToken") != "web-token":
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `<ErrorResponse><Error><Code>InvalidIdentityToken</Code><Message>bad token</Message></Error></ErrorResponse>`)
	default:
		fmt.Fprintf(w, stsXML, action, "ASIA"+action, "secret-"+action, "token-"+action)
	}
}

func TestProfileChainsThroughSTS(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	fake := &sts{}
	client, _ := steer(t, fake)
	dir := t.TempDir()
	tokenFile := write(t, filepath.Join(dir, "web.jwt"), "web-token\n")
	config := write(t, filepath.Join(dir, "config"), `[default]
region = us-west-2
aws_access_key_id = c1
aws_secret_access_key = c2

[profile assume]
role_arn = arn:aws:iam::1:role/x
source_profile = default
external_id = ext
role_session_name = sess
duration_seconds = 900
region = eu-central-1

[profile hop]
role_arn = arn:aws:iam::1:role/hop
source_profile = assume

[profile denied]
role_arn = arn:aws:iam::1:role/denied
source_profile = default

[profile mfa]
role_arn = arn:aws:iam::1:role/m
source_profile = default
mfa_serial = arn:aws:iam::1:mfa/me

[profile loop]
role_arn = arn:aws:iam::1:role/l
source_profile = loop

[profile selfkeyed]
aws_access_key_id = s1
aws_secret_access_key = s2
role_arn = arn:aws:iam::1:role/self

[profile dangling]
role_arn = arn:aws:iam::1:role/d

[profile fromenv]
role_arn = arn:aws:iam::1:role/e
credential_source = Environment

[profile oddsource]
role_arn = arn:aws:iam::1:role/o
credential_source = Moon

[profile web]
role_arn = arn:aws:iam::1:role/w
web_identity_token_file = `+tokenFile+`

[profile nested]
region = us-east-1
s3 =
  max_concurrent_requests = 20
  addressing_style = path
aws_access_key_id = n1
aws_secret_access_key = n2

[profile nothing]
region = us-east-1

[profile badduration]
role_arn = arn:aws:iam::1:role/x
source_profile = default
duration_seconds = soon
`)
	creds := write(t, filepath.Join(dir, "credentials"), "[default]\naws_access_key_id = k1\naws_secret_access_key = k2\n")
	profile := func(name string) Profile { return Profile{Name: name, File: creds, Config: config, Client: client} }
	if c, err := profile("default").Credentials(ctx); err != nil || c != (Credentials{AccessKey: "k1", SecretKey: "k2"}) {
		t.Fatalf("the credentials file must win over config: %v %+v", err, c)
	}
	if c, err := profile("nested").Credentials(ctx); err != nil || c.AccessKey != "n1" {
		t.Fatalf("nested settings must not break a profile: %v %+v", err, c)
	}
	c, err := profile("assume").Credentials(ctx)
	if err != nil || c.AccessKey != "ASIAAssumeRole" || c.SessionToken != "token-AssumeRole" || !c.Expires.Equal(time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("%v %+v", err, c)
	}
	form := fake.forms[0]
	if form.Get("RoleArn") != "arn:aws:iam::1:role/x" || form.Get("RoleSessionName") != "sess" || form.Get("ExternalId") != "ext" || form.Get("DurationSeconds") != "900" || form.Get("Version") != stsVersion {
		t.Fatalf("%v", form)
	}
	if !strings.Contains(fake.auths[0], "Credential=k1/") || !strings.Contains(fake.auths[0], "/eu-central-1/sts/aws4_request") || fake.hosts[0] != "sts.eu-central-1.amazonaws.com" {
		t.Fatalf("assume role must be signed with the source keys in the profile region: %s %s", fake.auths[0], fake.hosts[0])
	}
	if c, err := profile("hop").Credentials(ctx); err != nil || c.AccessKey != "ASIAAssumeRole" || !strings.Contains(fake.auths[2], "Credential=ASIAAssumeRole/") || !strings.HasPrefix(fake.forms[2].Get("RoleSessionName"), "icebath-") {
		t.Fatalf("a role may be assumed from another assumed role: %v %+v %s", err, c, fake.auths[2])
	}
	if c, err := profile("selfkeyed").Credentials(ctx); err != nil || c.AccessKey != "ASIAAssumeRole" || !strings.Contains(fake.auths[3], "Credential=s1/") || fake.hosts[3] != "sts.amazonaws.com" {
		t.Fatalf("a profile with its own keys assumes with them at the global endpoint: %v %+v %v", err, c, fake.hosts)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "e1")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "e2")
	if c, err := profile("fromenv").Credentials(ctx); err != nil || c.AccessKey != "ASIAAssumeRole" || !strings.Contains(fake.auths[4], "Credential=e1/") {
		t.Fatalf("credential_source Environment: %v %+v", err, c)
	}
	if c, err := profile("web").Credentials(ctx); err != nil || c.AccessKey != "ASIAAssumeRoleWithWebIdentity" || fake.auths[5] != "" || fake.forms[5].Get("WebIdentityToken") != "web-token" {
		t.Fatalf("web identity: %v %+v %v", err, c, fake.forms[5])
	}
	asked := ""
	withMFA := profile("mfa")
	withMFA.TokenCode = func(_ context.Context, serial string) (string, error) { asked = serial; return "123456", nil }
	if c, err := withMFA.Credentials(ctx); err != nil || c.AccessKey != "ASIAAssumeRole" || asked != "arn:aws:iam::1:mfa/me" || fake.forms[6].Get("SerialNumber") != asked || fake.forms[6].Get("TokenCode") != "123456" {
		t.Fatalf("mfa: %v %+v %v", err, c, fake.forms[6])
	}
	for name, want := range map[string]string{
		"mfa":         "TokenCode",
		"loop":        "refers back",
		"dangling":    "source_profile",
		"oddsource":   "Moon",
		"denied":      "AccessDenied: not for you",
		"nothing":     "names no credentials",
		"absent":      "not found",
		"badduration": "duration_seconds",
	} {
		if _, err := profile(name).Credentials(ctx); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::1:role/env")
	t.Setenv("AWS_ROLE_SESSION_NAME", "pod")
	t.Setenv("AWS_REGION", "ap-south-1")
	if c, err := (WebIdentity{Client: client}).Credentials(ctx); err != nil || c.AccessKey != "ASIAAssumeRoleWithWebIdentity" || fake.forms[len(fake.forms)-1].Get("RoleSessionName") != "pod" || fake.hosts[len(fake.hosts)-1] != "sts.ap-south-1.amazonaws.com" {
		t.Fatalf("env web identity: %v %+v", err, c)
	}
	t.Setenv("AWS_ROLE_ARN", "")
	if _, err := (WebIdentity{Client: client}).Credentials(ctx); err == nil {
		t.Fatal("web identity without a role accepted")
	}
	write(t, tokenFile, "wrong")
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::1:role/env")
	if _, err := (WebIdentity{Client: client}).Credentials(ctx); err == nil || !strings.Contains(err.Error(), "InvalidIdentityToken") {
		t.Fatalf("%v", err)
	}
}

func TestCredentialProcess(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	dir := t.TempDir()
	config := write(t, filepath.Join(dir, "config"), `[profile proc]
credential_process = echo '{"Version":1,"AccessKeyId":"p1","SecretAccessKey":"p2","SessionToken":"p3","Expiration":"2030-06-01T00:00:00Z"}'

[profile permanent]
credential_process = printf '{"Version":1,"AccessKeyId":"q1","SecretAccessKey":"q2"}'

[profile oldversion]
credential_process = echo '{"Version":2,"AccessKeyId":"p1","SecretAccessKey":"p2"}'

[profile crashes]
credential_process = echo nope >&2; exit 3

[profile garbage]
credential_process = echo not-json
`)
	p := Profile{Name: "proc", Config: config, File: filepath.Join(dir, "none")}
	c, err := p.Credentials(ctx)
	if err != nil || c != (Credentials{AccessKey: "p1", SecretKey: "p2", SessionToken: "p3", Expires: time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)}) {
		t.Fatalf("%v %+v", err, c)
	}
	p.Name = "permanent"
	if c, err := p.Credentials(ctx); err != nil || c != (Credentials{AccessKey: "q1", SecretKey: "q2"}) {
		t.Fatalf("%v %+v", err, c)
	}
	for name, want := range map[string]string{"oldversion": "version 2", "crashes": "nope", "garbage": "output"} {
		p.Name = name
		if _, err := p.Credentials(ctx); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func ssoCachePathFor(t *testing.T, key string) string {
	t.Helper()
	sum := sha1.Sum([]byte(key))
	return filepath.Join(os.Getenv("HOME"), ".aws", "sso", "cache", hex.EncodeToString(sum[:])+".json")
}

func TestSSOProfiles(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	var mu sync.Mutex
	var seen []string
	client, _ := steer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.Header.Get("X-Meant-For")+r.URL.Path+" "+r.Header.Get("x-amz-sso_bearer_token"))
		mu.Unlock()
		switch {
		case r.URL.Path == "/token":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["grantType"] != "refresh_token" || in["refreshToken"] != "r1" || in["clientId"] != "cid" || in["clientSecret"] != "csec" {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			io.WriteString(w, `{"accessToken":"renewed","refreshToken":"r2","expiresIn":3600,"tokenType":"Bearer"}`)
		case r.URL.Path == "/federation/credentials" && r.Header.Get("x-amz-sso_bearer_token") == "fresh" && r.URL.Query().Get("account_id") == "111" && r.URL.Query().Get("role_name") == "Admin":
			io.WriteString(w, `{"roleCredentials":{"accessKeyId":"sso1","secretAccessKey":"sso2","sessionToken":"sso3","expiration":1893456000000}}`)
		case r.URL.Path == "/federation/credentials" && r.Header.Get("x-amz-sso_bearer_token") == "renewed" && r.URL.Query().Get("account_id") == "222":
			io.WriteString(w, `{"roleCredentials":{"accessKeyId":"dev1","secretAccessKey":"dev2","sessionToken":"dev3","expiration":1893456000000}}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"message":"Session token not found or invalid"}`)
		}
	}))
	dir := t.TempDir()
	config := write(t, filepath.Join(dir, "config"), `[profile legacy]
sso_start_url = https://d-123.awsapps.com/start
sso_region = us-east-1
sso_account_id = 111
sso_role_name = Admin

[profile modern]
sso_session = corp
sso_account_id = 222
sso_role_name = Dev

[sso-session corp]
sso_start_url = https://corp.awsapps.com/start
sso_region = eu-west-1
sso_registration_scopes = sso:account:access

[profile orphan]
sso_session = gone
sso_account_id = 1
sso_role_name = r

[profile partial]
sso_start_url = https://x
sso_region = us-east-1
`)
	profile := func(name string) Profile {
		return Profile{Name: name, Config: config, File: filepath.Join(dir, "none"), Client: client}
	}
	if _, err := profile("legacy").Credentials(ctx); err == nil || !strings.Contains(err.Error(), "aws sso login --profile legacy") {
		t.Fatalf("no cache: %v", err)
	}
	write(t, ssoCachePathFor(t, "https://d-123.awsapps.com/start"), `{"accessToken":"fresh","expiresAt":"2031-01-01T00:00:00UTC","region":"us-east-1","startUrl":"https://d-123.awsapps.com/start"}`)
	c, err := profile("legacy").Credentials(ctx)
	if err != nil || c != (Credentials{AccessKey: "sso1", SecretKey: "sso2", SessionToken: "sso3", Expires: time.UnixMilli(1893456000000).UTC()}) {
		t.Fatalf("%v %+v", err, c)
	}
	if seen[0] != "GET portal.sso.us-east-1.amazonaws.com/federation/credentials fresh" {
		t.Fatalf("%v", seen)
	}
	modern := ssoCachePathFor(t, "corp")
	write(t, modern, `{"accessToken":"stale","expiresAt":"2020-01-01T00:00:00Z","refreshToken":"r1","clientId":"cid","clientSecret":"csec","registrationExpiresAt":"2031-01-01T00:00:00Z"}`)
	c, err = profile("modern").Credentials(ctx)
	if err != nil || c.AccessKey != "dev1" {
		t.Fatalf("%v %+v", err, c)
	}
	if seen[1] != "POST oidc.eu-west-1.amazonaws.com/token " || seen[2] != "GET portal.sso.eu-west-1.amazonaws.com/federation/credentials renewed" {
		t.Fatalf("%v", seen)
	}
	var cache ssoCache
	data, _ := os.ReadFile(modern)
	json.Unmarshal(data, &cache)
	if cache.AccessToken != "renewed" || cache.RefreshToken != "r2" || cache.Region != "eu-west-1" || !cache.ExpiresAt.After(time.Now().Add(50*time.Minute)) {
		t.Fatalf("the refreshed token must be written back: %s", data)
	}
	write(t, modern, `{"accessToken":"stale","expiresAt":"2020-01-01T00:00:00Z"}`)
	if _, err := profile("modern").Credentials(ctx); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("%v", err)
	}
	write(t, modern, `{"accessToken":"stale","expiresAt":"2020-01-01T00:00:00Z","refreshToken":"r1","clientId":"cid","clientSecret":"csec","registrationExpiresAt":"2020-01-01T00:00:00Z"}`)
	if _, err := profile("modern").Credentials(ctx); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("an expired registration cannot refresh: %v", err)
	}
	write(t, modern, `{"accessToken":"stale","expiresAt":"2020-01-01T00:00:00Z","refreshToken":"wrong","clientId":"cid","clientSecret":"csec"}`)
	if _, err := profile("modern").Credentials(ctx); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("%v", err)
	}
	write(t, ssoCachePathFor(t, "https://d-123.awsapps.com/start"), `{"accessToken":"other","expiresAt":"2031-01-01T00:00:00Z"}`)
	if _, err := profile("legacy").Credentials(ctx); err == nil || !strings.Contains(err.Error(), "Session token not found") {
		t.Fatalf("%v", err)
	}
	for name, want := range map[string]string{"orphan": "sso-session", "partial": "sso_account_id"} {
		if _, err := profile(name).Credentials(ctx); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestContainerAndInstanceMetadata(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	var mu sync.Mutex
	var auths []string
	container := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.URL.Path != "/v2/creds" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, `{"AccessKeyId":"ct1","SecretAccessKey":"ct2","Token":"ct3","Expiration":"2031-01-01T00:00:00Z"}`)
	}))
	defer container.Close()
	if _, err := (Container{}).Credentials(ctx); err == nil || !strings.Contains(err.Error(), "AWS_CONTAINER_CREDENTIALS") {
		t.Fatalf("%v", err)
	}
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", container.URL+"/v2/creds")
	t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN", "Bearer env-token")
	c, err := (Container{}).Credentials(ctx)
	if err != nil || c != (Credentials{AccessKey: "ct1", SecretKey: "ct2", SessionToken: "ct3", Expires: time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)}) || auths[0] != "Bearer env-token" {
		t.Fatalf("%v %+v %v", err, c, auths)
	}
	tokenFile := write(t, filepath.Join(t.TempDir(), "token"), "Bearer file-token\n")
	t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", tokenFile)
	if _, err := (Container{}).Credentials(ctx); err != nil || auths[1] != "Bearer file-token" {
		t.Fatalf("%v %v", err, auths)
	}
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", container.URL+"/missing")
	if _, err := (Container{}).Credentials(ctx); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("%v", err)
	}
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "/v2/creds")
	unreachable := &http.Client{Transport: rewrite{to: must(url.Parse(container.URL))}}
	if c, err := (Container{Client: unreachable}).Credentials(ctx); err != nil || c.AccessKey != "ct1" {
		t.Fatalf("relative uri must resolve against the task endpoint: %v", err)
	}

	var steps []string
	imds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		steps = append(steps, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			if r.Header.Get("X-Aws-Ec2-Metadata-Token-Ttl-Seconds") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			io.WriteString(w, "imds-token")
		case r.Header.Get("X-Aws-Ec2-Metadata-Token") != "imds-token":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/":
			io.WriteString(w, "app-role\n")
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/app-role":
			io.WriteString(w, `{"Code":"Success","LastUpdated":"2030-01-01T00:00:00Z","Type":"AWS-HMAC","AccessKeyId":"im1","SecretAccessKey":"im2","Token":"im3","Expiration":"2031-01-01T06:00:00Z"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer imds.Close()
	if _, err := (IMDS{}).Credentials(ctx); err == nil || !strings.Contains(err.Error(), "AWS_EC2_METADATA_DISABLED") {
		t.Fatalf("%v", err)
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "")
	t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", imds.URL+"/")
	c, err = (IMDS{}).Credentials(ctx)
	if err != nil || c != (Credentials{AccessKey: "im1", SecretKey: "im2", SessionToken: "im3", Expires: time.Date(2031, 1, 1, 6, 0, 0, 0, time.UTC)}) {
		t.Fatalf("%v %+v", err, c)
	}
	if strings.Join(steps, ",") != "PUT /latest/api/token,GET /latest/meta-data/iam/security-credentials/,GET /latest/meta-data/iam/security-credentials/app-role" {
		t.Fatalf("%v", steps)
	}
	t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", imds.URL+"/elsewhere")
	if _, err := (IMDS{}).Credentials(ctx); err == nil {
		t.Fatal("a metadata service without a token endpoint accepted")
	}
	dir := t.TempDir()
	config := write(t, filepath.Join(dir, "config"), "[profile ec2]\nrole_arn = arn:aws:iam::1:role/x\ncredential_source = Ec2InstanceMetadata\n\n[profile ecs]\nrole_arn = arn:aws:iam::1:role/y\ncredential_source = EcsContainer\n")
	t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", imds.URL)
	fake := &sts{}
	stsClient, _ := steer(t, fake)
	t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", container.URL+"/v2/creds")
	for name, source := range map[string]string{"ec2": "im1", "ecs": "ct1"} {
		p := Profile{Name: name, Config: config, File: filepath.Join(dir, "none"), Client: stsClient}
		if c, err := p.Credentials(ctx); err != nil || c.AccessKey != "ASIAAssumeRole" || !strings.Contains(fake.auths[len(fake.auths)-1], "Credential="+source+"/") {
			t.Fatalf("%s: %v %+v %v", name, err, c, fake.auths)
		}
	}
}

func TestSTSResultParsing(t *testing.T) {
	if _, err := stsResult([]byte(`<AssumeRoleResponse><AssumeRoleResult></AssumeRoleResult></AssumeRoleResponse>`)); err == nil {
		t.Fatal("response without credentials accepted")
	}
	if _, err := stsResult([]byte(`<AssumeRoleResponse><AssumeRoleResult><Credentials><AccessKeyId>a</AccessKeyId></Credentials></AssumeRoleResult></AssumeRoleResponse>`)); err == nil {
		t.Fatal("incomplete credentials accepted")
	}
	if _, err := stsResult([]byte(`<AssumeRoleResponse><AssumeRoleResult><Credentials><AccessKeyId>a</AccessKeyId><SecretAccessKey>b</SecretAccessKey><SessionToken>c</SessionToken><Expiration>soon</Expiration></Credentials></AssumeRoleResult></AssumeRoleResponse>`)); err == nil {
		t.Fatal("unparsable expiration accepted")
	}
	if endpoint, region := stsEndpoint(""); endpoint != "https://sts.amazonaws.com/" || region != "us-east-1" {
		t.Fatal(endpoint, region)
	}
	var parsed ssoTime
	for _, raw := range []string{`"2031-01-01T00:00:00Z"`, `"2031-01-01T00:00:00UTC"`} {
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil || !parsed.Equal(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("%s: %v %v", raw, err, parsed)
		}
	}
	if err := json.Unmarshal([]byte(`""`), &parsed); err != nil || !parsed.IsZero() {
		t.Fatalf("%v", err)
	}
	if err := json.Unmarshal([]byte(`"yesterday"`), &parsed); err == nil {
		t.Fatal("bad time accepted")
	}
	if out, _ := json.Marshal(ssoTime{}); string(out) != `""` {
		t.Fatal(string(out))
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
