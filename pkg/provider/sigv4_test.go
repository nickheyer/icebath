package provider

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var sigV4TestTime = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

var sigV4TestCreds = awsCredentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}

func sigV4TestRequest(t *testing.T, method, rawURL, body string, headers map[string]string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != "" {
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	return req
}

func TestSigV4IAMListUsersDocumentationVector(t *testing.T) {
	req := sigV4TestRequest(t, http.MethodGet, "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", "",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded; charset=utf-8"})
	res, err := sigV4Sign(req, nil, sigV4TestCreds, "iam", "us-east-1", sigV4TestTime)
	if err != nil {
		t.Fatal(err)
	}
	wantCanonical := "GET\n/\nAction=ListUsers&Version=2010-05-08\ncontent-type:application/x-www-form-urlencoded; charset=utf-8\n" +
		"host:iam.amazonaws.com\nx-amz-date:20150830T123600Z\n\ncontent-type;host;x-amz-date\n" + sigV4EmptyPayloadSHA
	if res.CanonicalRequest != wantCanonical {
		t.Fatalf("canonical request:\n%s\nwant:\n%s", res.CanonicalRequest, wantCanonical)
	}
	if got := sha256Hex([]byte(res.CanonicalRequest)); got != "f536975d06c0309214f805bb90ccff089219ecd68b2577efef23edd43b7e1a59" {
		t.Fatalf("canonical request hash %s", got)
	}
	if got := hex.EncodeToString(sigV4SigningKey(sigV4TestCreds.SecretAccessKey, "20150830", "us-east-1", "iam")); got != "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9" {
		t.Fatalf("kSigning %s", got)
	}
	if res.Signature != "5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7" {
		t.Fatalf("signature %s", res.Signature)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("authorization %q", got)
	}
	if req.Header.Get("X-Amz-Date") != "20150830T123600Z" {
		t.Fatalf("x-amz-date %q", req.Header.Get("X-Amz-Date"))
	}
}

func TestSigV4MatchesIndependentSignerVectors(t *testing.T) {
	cases := []struct {
		name, method, url, body, token, service, region, signed, sig string
		headers                                                      map[string]string
	}{
		{"post vanilla", http.MethodPost, "https://example.amazonaws.com/", "", "", "service", "us-east-1", "host;x-amz-date",
			"5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b", nil},
		{"get vanilla", http.MethodGet, "https://example.amazonaws.com/", "", "", "service", "us-east-1", "host;x-amz-date",
			"5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31", nil},
		{"bedrock invoke encoded model id", http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/us.anthropic.claude-sonnet-4-5-20250929-v1%3A0/invoke",
			`{"messages":[]}`, "", "bedrock", "us-east-1", "accept;content-length;content-type;host;x-amz-date",
			"877e80091ffd4e91e2bfc55b431238a6e02d7b6afd24d48b6711e42f24caa44d", map[string]string{"Content-Type": "application/json", "Accept": "application/json"}},
		{"session token and raw colon", http.MethodPost, "https://bedrock-runtime.us-west-2.amazonaws.com/model/anthropic.claude-v2:1/invoke-with-response-stream",
			`{"a":1}`, "SESSIONTOKEN", "bedrock", "us-west-2", "content-length;content-type;host;x-amz-date;x-amz-security-token",
			"39becf0f055ccdac99189360a86606a55b30e890de3deef7973120e33a659a0b", map[string]string{"Content-Type": "application/json"}},
		{"query sorted and re-encoded", http.MethodGet, "https://example.amazonaws.com/?Param2=value2&Param1=value1&b=%20x&a=1", "", "", "service", "us-east-1",
			"host;x-amz-date", "740758b4806c435fed81330adcfffbe1c669a91cc14b04744583ed8558ffa43e", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := sigV4TestRequest(t, tc.method, tc.url, tc.body, tc.headers)
			creds := sigV4TestCreds
			creds.SessionToken = tc.token
			res, err := sigV4Sign(req, []byte(tc.body), creds, tc.service, tc.region, sigV4TestTime)
			if err != nil {
				t.Fatal(err)
			}
			if res.SignedHeaders != tc.signed || res.Signature != tc.sig {
				t.Fatalf("signed %q sig %s\ncanonical:\n%s", res.SignedHeaders, res.Signature, res.CanonicalRequest)
			}
			if tc.token != "" && req.Header.Get("X-Amz-Security-Token") != tc.token {
				t.Fatalf("security token header %q", req.Header.Get("X-Amz-Security-Token"))
			}
		})
	}
}

func TestSigV4CanonicalPathAndHeaders(t *testing.T) {
	for in, want := range map[string]string{"": "/", "/": "/", "/a/./b/../c": "/a/c", "//a//b/": "/a/b/", "/..": "/", "/a/b/..": "/a"} {
		if got := sigV4RemoveDotSegments(in); got != want {
			t.Errorf("remove dot segments %q = %q want %q", in, got, want)
		}
	}
	req := sigV4TestRequest(t, http.MethodPost, "https://host.example:443/model/a%3Ab/invoke", "", map[string]string{
		"X-Custom": "  a   b  ", "User-Agent": "skip", "Expect": "100-continue", "X-Amzn-Trace-Id": "skip"})
	names, values := sigV4HeadersToSign(req, nil)
	if strings.Join(names, ";") != "host;x-custom" || values["x-custom"] != "a b" || values["host"] != "host.example" {
		t.Fatalf("headers %v %v", names, values)
	}
	if got := sigV4CanonicalURI(req.URL); got != "/model/a%253Ab/invoke" {
		t.Fatalf("canonical uri %q", got)
	}
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	res, _ := sigV4Sign(req, []byte("ignored"), sigV4TestCreds, "bedrock", "us-east-1", sigV4TestTime)
	if !strings.HasSuffix(res.CanonicalRequest, "\nUNSIGNED-PAYLOAD") {
		t.Fatalf("explicit payload hash not used:\n%s", res.CanonicalRequest)
	}
	if _, err := sigV4Sign(req, nil, awsCredentials{AccessKeyID: "only"}, "bedrock", "us-east-1", sigV4TestTime); err == nil {
		t.Fatal("signing without a secret must fail")
	}
}

func TestSigV4RoundTripperSignsAndDropsSentinelAuth(t *testing.T) {
	var gotAuth, gotBody string
	var verifyErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		gotAuth, gotBody = r.Header.Get("Authorization"), string(data)
		_, verifyErr = bedrockVerifySigV4(r, data, map[string]string{"AKIDEXAMPLE": sigV4TestCreds.SecretAccessKey})
		w.WriteHeader(204)
	}))
	defer srv.Close()
	rt := &sigV4RoundTripper{creds: awsStaticProvider{sigV4TestCreds}, service: "bedrock", region: "us-east-1",
		drop: []string{"Authorization", "X-Amz-Date", "X-Amz-Security-Token"}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/openai/v1/responses", strings.NewReader(`{"model":"openai.gpt-5.5"}`))
	req.Header.Set("Authorization", "Bearer aws-sdk")
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: rt}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") || strings.Contains(gotAuth, "aws-sdk") {
		t.Fatalf("authorization %q", gotAuth)
	}
	if gotBody != `{"model":"openai.gpt-5.5"}` || verifyErr != nil {
		t.Fatalf("body %q verify %v", gotBody, verifyErr)
	}
	if req.Header.Get("Authorization") != "Bearer aws-sdk" {
		t.Fatal("round tripper must not mutate the caller's request")
	}
}
