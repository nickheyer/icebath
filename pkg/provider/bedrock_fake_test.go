package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	bedrockFakeAccessKey = "AKIAFAKEE2EBEDROCK01"
	bedrockFakeSecretKey = "fake/e2e/secret/never+real/0000000000000"
	bedrockFakeRegion    = "us-east-1"
)

var (
	bedrockFakeAuthRe = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=([^/]+)/(\d{8})/([a-z0-9-]+)/([a-z0-9-]+)/aws4_request, ?SignedHeaders=([a-z0-9;-]+), ?Signature=([0-9a-f]{64})$`)
)

// Rejection carries the AWS error code and message the fake answers with
type bedrockRejection struct{ Code, Message string }

func (e *bedrockRejection) Error() string { return e.Code + ": " + e.Message }

// Recomputes a SigV4 signature with the known secret of its access key and returns the credential scope
func bedrockVerifySigV4(r *http.Request, body []byte, secrets map[string]string) (map[string]string, error) {
	auth := r.Header.Get("Authorization")
	m := bedrockFakeAuthRe.FindStringSubmatch(auth)
	if m == nil {
		return nil, &bedrockRejection{"MissingAuthenticationTokenException", fmt.Sprintf("Missing or malformed SigV4 Authorization: %q", auth)}
	}
	scope := map[string]string{"key": m[1], "date": m[2], "region": m[3], "service": m[4], "signed": m[5], "sig": m[6]}
	secret, ok := secrets[m[1]]
	if !ok || m[4] != "bedrock" {
		return nil, &bedrockRejection{"UnrecognizedClientException", "The security token included in the request is invalid."}
	}
	only := map[string]bool{}
	for _, h := range strings.Split(m[5], ";") {
		only[h] = true
	}
	ts, err := time.Parse(sigV4TimeFormat, r.Header.Get("X-Amz-Date"))
	if err != nil {
		return nil, &bedrockRejection{"IncompleteSignatureException", "X-Amz-Date is missing or malformed"}
	}
	clone := r.Clone(r.Context())
	if clone.URL.Host == "" {
		clone.URL.Host = r.Host
	}
	got := sigV4Compute(clone, sigV4PayloadHash(clone, body), awsCredentials{AccessKeyID: m[1], SecretAccessKey: secret}, "bedrock", m[3], ts, only)
	if got.Signature != m[6] {
		return nil, &bedrockRejection{"InvalidSignatureException", "The request signature we calculated does not match the signature you provided."}
	}
	return scope, nil
}

// ---- botocore ParamValidator over the Converse input shapes ----

// Python repr botocore prints for an invalid value
func bedrockPyRepr(v any) string {
	switch v := v.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return "'" + v + "'"
	case json.Number:
		return string(v)
	}
	data, _ := json.Marshal(v)
	return string(data)
}

// ---- Converse conversation rules beyond the schema ----
