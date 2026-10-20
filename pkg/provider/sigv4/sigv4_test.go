package sigv4

import (
	"bytes"
	"context"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
)

var (
	exampleCreds = Credentials{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	exampleTime  = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
)

// Worked example from the AWS signing documentation for IAM ListUsers
func TestSignMatchesTheDocumentedExample(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	if err := Sign(r, exampleCreds, "us-east-1", "iam", exampleTime); err != nil {
		t.Fatal(err)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	if got := r.Header.Get("Authorization"); got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if r.Header.Get("X-Amz-Date") != "20150830T123600Z" || r.Header.Get("X-Amz-Security-Token") != "" {
		t.Fatalf("%v", r.Header)
	}
}

func TestSignCoversBodyTokenAndEscapedPaths(t *testing.T) {
	body := []byte(`{"messages":[]}`)
	r, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-west-2.amazonaws.com/model/anthropic.claude%3A0/converse", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	creds := exampleCreds
	creds.SessionToken = "tok"
	Sign(r, creds, "us-west-2", "bedrock", exampleTime)
	auth := r.Header.Get("Authorization")
	if !strings.Contains(auth, "SignedHeaders=content-type;host;x-amz-date;x-amz-security-token,") || r.Header.Get("X-Amz-Security-Token") != "tok" {
		t.Fatal(auth)
	}
	sent, _ := io.ReadAll(r.Body)
	if string(sent) != string(body) {
		t.Fatal("body consumed by signing")
	}
	other, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-west-2.amazonaws.com/model/anthropic.claude%3A0/converse", bytes.NewReader([]byte(`{"messages":[1]}`)))
	other.Header.Set("Content-Type", "application/json")
	Sign(other, creds, "us-west-2", "bedrock", exampleTime)
	if other.Header.Get("Authorization") == auth {
		t.Fatal("different bodies must sign differently")
	}
	if canonicalPath(r.URL) != "/model/anthropic.claude%253A0/converse" {
		t.Fatal(canonicalPath(r.URL))
	}
	empty, _ := http.NewRequest(http.MethodGet, "https://x.amazonaws.com", nil)
	if canonicalPath(empty.URL) != "/" {
		t.Fatal(canonicalPath(empty.URL))
	}
	u, _ := http.NewRequest(http.MethodGet, "https://x.amazonaws.com/?b=2&a=x%20y&a=1&c~=%2F", nil)
	if q := canonicalQuery(u.URL.Query()); q != "a=1&a=x%20y&b=2&c~=%2F" {
		t.Fatal(q)
	}
	stream, _ := http.NewRequest(http.MethodPost, "https://x.amazonaws.com/", io.NopCloser(strings.NewReader("abc")))
	if err := Sign(stream, creds, "r", "s", exampleTime); err != nil {
		t.Fatal(err)
	}
	again, _ := io.ReadAll(stream.Body)
	if string(again) != "abc" {
		t.Fatal("unknown body must be restored")
	}
	broken, _ := http.NewRequest(http.MethodPost, "https://x.amazonaws.com/", io.NopCloser(failing{errors.New("disk gone")}))
	if err := Sign(broken, creds, "r", "s", exampleTime); err == nil || !strings.Contains(err.Error(), "disk gone") {
		t.Fatalf("%v", err)
	}
	if err := Sign(r, Credentials{}, "r", "s", exampleTime); err == nil {
		t.Fatal("empty credentials signed")
	}
}

func TestApplyPoisonsUnsignableRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "reached") }))
	defer srv.Close()
	a := &Auth{Region: "r", Service: "s", Source: Static(exampleCreds)}
	a.Refresh(context.Background())
	req, _ := http.NewRequest(http.MethodPost, srv.URL, io.NopCloser(failing{errors.New("disk gone")}))
	a.Apply(req)
	if _, err := srv.Client().Do(req); err == nil || !strings.Contains(err.Error(), "disk gone") {
		t.Fatalf("unsignable request must fail on send: %v", err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("unsignable request must carry no signature")
	}
	good, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("ok"))
	a.Apply(good)
	resp, err := srv.Client().Do(good)
	if err != nil || resp.StatusCode != 200 || good.Header.Get("Authorization") == "" {
		t.Fatalf("%v", err)
	}
	resp.Body.Close()
}

// Points every credential lookup at an empty home and silences the remote sources
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_ROLE_SESSION_NAME", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "AWS_EC2_METADATA_SERVICE_ENDPOINT", "AWS_REGION", "AWS_DEFAULT_REGION"} {
		t.Setenv(name, "")
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

func TestSources(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	if _, err := (Static{}).Credentials(ctx); err == nil {
		t.Fatal("empty static accepted")
	}
	if c, err := Static(exampleCreds).Credentials(ctx); err != nil || c != exampleCreds {
		t.Fatalf("%v %+v", err, c)
	}
	if _, err := (Env{}).Credentials(ctx); err == nil {
		t.Fatal("empty env accepted")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")
	t.Setenv("AWS_SESSION_TOKEN", "st")
	if c, err := (Env{}).Credentials(ctx); err != nil || c != (Credentials{AccessKey: "ak", SecretKey: "sk", SessionToken: "st"}) {
		t.Fatalf("%v %+v", err, c)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "credentials")
	os.WriteFile(file, []byte("# comment\n[default]\naws_access_key_id = d1\naws_secret_access_key=d2\n\n[profile work]\nAWS_ACCESS_KEY_ID = w1\naws_secret_access_key = w2\naws_session_token = w3\n[broken]\naws_access_key_id = only\n"), 0o600)
	if c, err := (Profile{File: file}).Credentials(ctx); err != nil || c.AccessKey != "d1" || c.SecretKey != "d2" {
		t.Fatalf("%v %+v", err, c)
	}
	if c, err := (Profile{Name: "work", File: file}).Credentials(ctx); err != nil || c != (Credentials{AccessKey: "w1", SecretKey: "w2", SessionToken: "w3"}) {
		t.Fatalf("%v %+v", err, c)
	}
	t.Setenv("AWS_PROFILE", "work")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", file)
	if c, err := (Profile{}).Credentials(ctx); err != nil || c.AccessKey != "w1" {
		t.Fatalf("%v %+v", err, c)
	}
	for name, p := range map[string]Profile{"missing profile": {Name: "nope", File: file}, "broken": {Name: "broken", File: file}, "missing files": {File: filepath.Join(dir, "none"), Config: filepath.Join(dir, "none2")}} {
		if _, err := p.Credentials(ctx); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	if c, err := Default().Credentials(ctx); err != nil || c.AccessKey != "w1" {
		t.Fatalf("default chain %v %+v", err, c)
	}
	t.Setenv("AWS_PROFILE", "nope")
	_, err := Default().Credentials(ctx)
	for _, want := range []string{"AWS_ACCESS_KEY_ID", "AWS_WEB_IDENTITY_TOKEN_FILE", "nope", "AWS_CONTAINER_CREDENTIALS", "AWS_EC2_METADATA_DISABLED"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("chain must report every failure, missing %s: %v", want, err)
		}
	}
	if _, err := Chain().Credentials(ctx); err == nil {
		t.Fatal("empty chain accepted")
	}
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")
	if Region() != "eu-west-1" {
		t.Fatal(Region())
	}
	t.Setenv("AWS_REGION", "us-east-2")
	if Region() != "us-east-2" {
		t.Fatal(Region())
	}
}

type counting struct {
	calls int
	creds Credentials
	err   error
}

func (c *counting) Credentials(context.Context) (Credentials, error) {
	c.calls++
	return c.creds, c.err
}

func TestAuthRefreshesOnExpiry(t *testing.T) {
	ctx := context.Background()
	if err := (&Auth{}).Refresh(ctx); err == nil {
		t.Fatal("unconfigured auth accepted")
	}
	now := exampleTime
	src := &counting{creds: Credentials{AccessKey: "a", SecretKey: "s", Expires: now.Add(10 * time.Minute)}}
	a := &Auth{Region: "us-east-1", Service: "bedrock", Source: src, Now: func() time.Time { return now }}
	if err := a.Refresh(ctx); err != nil || src.calls != 1 {
		t.Fatalf("%v %d", err, src.calls)
	}
	if err := a.Refresh(ctx); err != nil || src.calls != 1 {
		t.Fatal("fresh credentials must be kept")
	}
	now = now.Add(9*time.Minute + 30*time.Second)
	if err := a.Refresh(ctx); err != nil || src.calls != 2 {
		t.Fatalf("credentials about to expire must refresh: %v %d", err, src.calls)
	}
	src.err = errors.New("sts down")
	now = now.Add(time.Hour)
	if err := a.Refresh(ctx); err == nil || !strings.Contains(err.Error(), "sts down") {
		t.Fatalf("%v", err)
	}
	permanent := &Auth{Region: "r", Service: "s", Source: &counting{creds: exampleCreds}}
	permanent.Refresh(ctx)
	permanent.Refresh(ctx)
	if permanent.Source.(*counting).calls != 1 {
		t.Fatal("permanent credentials must load once")
	}
}

type echoProtocol struct{}

func (echoProtocol) ID() protocol.ID { return "echo" }
func (echoProtocol) Encode(req protocol.Request) (protocol.Call, error) {
	return protocol.Call{Method: http.MethodPost, Path: "/model/" + req.Model + "/converse", Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{}`)}, nil
}
func (echoProtocol) Decode(r io.Reader) (protocol.Response, error) {
	b, _ := io.ReadAll(r)
	return protocol.Response{Content: []protocol.Part{protocol.Text{Text: string(b)}}}, nil
}
func (echoProtocol) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(func(protocol.Event, error) bool) {}
}
func (echoProtocol) DecodeError(status int, body []byte) *protocol.Error {
	return &protocol.Error{Status: status, Message: string(body)}
}

func TestProviderSignsEveryRequest(t *testing.T) {
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if r.Header.Get("X-Amz-Date") == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		io.WriteString(w, "signed")
	}))
	defer srv.Close()
	auth := &Auth{Region: "us-east-1", Service: "bedrock", Source: Static(exampleCreds)}
	p := &provider.Provider{Name: "bedrock", Protocol: echoProtocol{}, BaseURL: srv.URL, Auth: auth}
	resp, err := p.Complete(context.Background(), protocol.Request{Model: "m"})
	if err != nil || resp.Content[0].(protocol.Text).Text != "signed" || len(auths) != 1 {
		t.Fatalf("%v %+v %v", err, resp, auths)
	}
	broken := &provider.Provider{Name: "bedrock", Protocol: echoProtocol{}, BaseURL: srv.URL, Auth: &Auth{Region: "r", Service: "s", Source: Static{}}}
	if _, err := broken.Complete(context.Background(), protocol.Request{Model: "m"}); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("%v", err)
	}
}
