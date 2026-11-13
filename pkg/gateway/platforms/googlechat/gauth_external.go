package googlechat

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	stsGrantType          = "urn:ietf:params:oauth:grant-type:token-exchange"
	stsRequestedTokenType = "urn:ietf:params:oauth:token-type:access_token"
	awsSubjectTokenType   = "urn:ietf:params:aws:token-type:aws4_request"
	awsDefaultVerifyURL   = "https://sts.{region}.amazonaws.com?Action=GetCallerIdentity&Version=2011-06-15"
)

var (
	workforcePoolAudience = regexp.MustCompile(`^//iam\.googleapis\.com/locations/[^/]+/workforcePools/`)
	awsEnvironmentID      = regexp.MustCompile(`^aws(\d+)$`)
)

// Subject token reader of one external account credential source
type subjectTokenSupplier func(ctx context.Context) (string, error)

// Error text of an STS error body
func stsErrorText(raw []byte) string {
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil || data["error"] == nil {
		return strings.TrimSpace(string(raw))
	}
	out := fmt.Sprintf("error code %v", data["error"])
	if d, ok := data["error_description"]; ok {
		out += fmt.Sprintf(": %v", d)
	}
	if u, ok := data["error_uri"]; ok {
		out += fmt.Sprintf(" - %v", u)
	}
	return out
}

// Posts an STS form request with optional basic client authentication
func postSTS(ctx context.Context, tokenURL string, form url.Values, clientID, clientSecret string, client *http.Client) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if clientID != "" && clientSecret != "" {
		req.SetBasicAuth(clientID, clientSecret)
	}
	if client == nil {
		client = authHTTPClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &tokenError{msg: stsErrorText(raw)}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &tokenError{msg: strings.TrimSpace(string(raw))}
	}
	return out, nil
}

// Workload or workforce identity federation exchanging a subject token at STS, optionally impersonating a service account
func externalAccountCredentials(info map[string]any, scopes []string) (*googleCredentials, error) {
	audience, subjectType := str(info, "audience"), str(info, "subject_token_type")
	src, _ := info["credential_source"].(map[string]any)
	if audience == "" || subjectType == "" || src == nil {
		return nil, errors.New("external account info is missing audience, subject_token_type or credential_source")
	}
	universe := str(info, "universe_domain")
	if universe == "" {
		universe = defaultUniverse
	}
	tokenURL := str(info, "token_url")
	workforceProject := str(info, "workforce_pool_user_project")
	if workforceProject != "" && !workforcePoolAudience.MatchString(audience) {
		return nil, errors.New("workforce_pool_user_project is only valid for workforce pool credentials")
	}
	clientID, clientSecret := str(info, "client_id"), str(info, "client_secret")
	impURL := str(info, "service_account_impersonation_url")
	lifetime := tokenLifetime
	if imp, ok := info["service_account_impersonation"].(map[string]any); ok {
		if secs, ok := imp["token_lifetime_seconds"].(float64); ok {
			lifetime = time.Duration(secs) * time.Second
		}
	}
	var supplier subjectTokenSupplier
	var stsClient *http.Client
	var err error
	_, executable := src["executable"]
	switch {
	case subjectType == awsSubjectTokenType:
		supplier, err = awsSubjectToken(src, audience)
	case executable:
		supplier, err = executableSubjectToken(src, audience, subjectType, impURL)
	default:
		supplier, stsClient, err = identityPoolSubjectToken(src)
		if err == nil && stsClient != nil && tokenURL == "" {
			tokenURL = "https://sts.mtls." + universe + "/v1/token"
		}
	}
	if err != nil {
		return nil, err
	}
	if tokenURL == "" {
		tokenURL = "https://sts." + universe + "/v1/token"
	}
	exchange := func(ctx context.Context, scopes []string) (string, time.Time, error) {
		now := time.Now().UTC()
		subject, err := supplier(ctx)
		if err != nil {
			return "", time.Time{}, err
		}
		form := url.Values{"grant_type": {stsGrantType}, "audience": {audience}, "requested_token_type": {stsRequestedTokenType},
			"subject_token": {subject}, "subject_token_type": {subjectType}}
		if len(scopes) > 0 {
			form.Set("scope", strings.Join(scopes, " "))
		}
		if workforceProject != "" && clientID == "" {
			opts, _ := json.Marshal(map[string]string{"userProject": workforceProject})
			form.Set("options", string(opts))
		}
		data, err := postSTS(ctx, tokenURL, form, clientID, clientSecret, stsClient)
		if err != nil {
			return "", time.Time{}, err
		}
		return tokenFrom(data, now)
	}
	c := &googleCredentials{kind: "external_account", quotaProject: str(info, "quota_project_id")}
	if impURL == "" {
		c.mint = func(ctx context.Context) (string, time.Time, error) { return exchange(ctx, scopes) }
		return c, nil
	}
	if _, err := impersonationTarget(impURL); err != nil {
		return nil, err
	}
	source := &googleCredentials{kind: "external_account"}
	source.mint = func(ctx context.Context) (string, time.Time, error) {
		return exchange(ctx, []string{cloudPlatformScope})
	}
	c.mint = impersonatedCredentials(source, impURL, nil, scopes, lifetime).mint
	return c, nil
}

// Subject token from a text or JSON payload as the credential source format declares it
func parseSubjectToken(payload []byte, format map[string]any, where string) (string, error) {
	if typ := str(format, "type"); typ == "" || typ == "text" {
		if len(payload) == 0 {
			return "", fmt.Errorf("missing subject token in %s", where)
		}
		return string(payload), nil
	}
	field := str(format, "subject_token_field_name")
	var data map[string]any
	if json.Unmarshal(payload, &data) != nil {
		return "", fmt.Errorf("unable to parse subject token from %s using key %q", where, field)
	}
	token, ok := data[field].(string)
	if !ok || token == "" {
		return "", fmt.Errorf("unable to parse subject token from %s using key %q", where, field)
	}
	return token, nil
}

// Identity pool subject token from a file, a URL or an X.509 workload certificate with its mTLS STS client
func identityPoolSubjectToken(src map[string]any) (subjectTokenSupplier, *http.Client, error) {
	if _, ok := src["environment_id"]; ok {
		return nil, nil, errors.New("identity pool credential_source must not set environment_id")
	}
	file, rawURL := str(src, "file"), str(src, "url")
	cert, hasCert := src["certificate"].(map[string]any)
	count := 0
	for _, set := range []bool{file != "", rawURL != "", hasCert} {
		if set {
			count++
		}
	}
	if count != 1 {
		return nil, nil, errors.New("credential_source needs exactly one of file, url or certificate")
	}
	format, _ := src["format"].(map[string]any)
	if typ := str(format, "type"); typ != "" && typ != "text" && typ != "json" {
		return nil, nil, fmt.Errorf("invalid credential_source format %q", typ)
	}
	if str(format, "type") == "json" && str(format, "subject_token_field_name") == "" {
		return nil, nil, errors.New("json credential_source format needs subject_token_field_name")
	}
	switch {
	case file != "":
		return func(context.Context) (string, error) {
			raw, err := os.ReadFile(file)
			if err != nil {
				return "", fmt.Errorf("subject token file %s was not found", file)
			}
			return parseSubjectToken(raw, format, file)
		}, nil, nil
	case rawURL != "":
		headers, _ := src["headers"].(map[string]any)
		return func(ctx context.Context) (string, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
			if err != nil {
				return "", err
			}
			for k, v := range headers {
				req.Header.Set(k, fmt.Sprint(v))
			}
			resp, err := authHTTPClient().Do(req)
			if err != nil {
				return "", err
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return "", fmt.Errorf("subject token URL returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
			}
			return parseSubjectToken(raw, format, rawURL)
		}, nil, nil
	}
	return x509SubjectToken(cert)
}

// Workload certificate chain as the subject token, with an STS client presenting that certificate
func x509SubjectToken(cert map[string]any) (subjectTokenSupplier, *http.Client, error) {
	useDefault, _ := cert["use_default_certificate_config"].(bool)
	location := str(cert, "certificate_config_location")
	if useDefault == (location != "") {
		return nil, nil, errors.New("certificate source needs either use_default_certificate_config or certificate_config_location")
	}
	if location == "" {
		location = os.Getenv("GOOGLE_API_CERTIFICATE_CONFIG")
	}
	if location == "" {
		location = filepath.Join(cloudSDKConfigDir(), "certificate_config.json")
	}
	raw, err := os.ReadFile(location)
	if err != nil {
		return nil, nil, fmt.Errorf("certificate config file %s was not found", location)
	}
	var conf struct {
		CertConfigs struct {
			Workload struct {
				CertPath string `json:"cert_path"`
				KeyPath  string `json:"key_path"`
			} `json:"workload"`
		} `json:"cert_configs"`
	}
	if err := json.Unmarshal(raw, &conf); err != nil || conf.CertConfigs.Workload.CertPath == "" || conf.CertConfigs.Workload.KeyPath == "" {
		return nil, nil, fmt.Errorf("certificate config file %s has no workload cert_path and key_path", location)
	}
	pair, err := tls.LoadX509KeyPair(conf.CertConfigs.Workload.CertPath, conf.CertConfigs.Workload.KeyPath)
	if err != nil {
		return nil, nil, err
	}
	chain := []string{base64.StdEncoding.EncodeToString(pair.Certificate[0])}
	if trust := str(cert, "trust_chain_path"); trust != "" {
		pemChain, err := os.ReadFile(trust)
		if err != nil {
			return nil, nil, fmt.Errorf("trust chain file %s was not found", trust)
		}
		for i := 0; ; i++ {
			var block *pem.Block
			if block, pemChain = pem.Decode(pemChain); block == nil {
				break
			}
			if bytes.Equal(block.Bytes, pair.Certificate[0]) {
				if i != 0 {
					return nil, nil, errors.New("the leaf certificate must be first in the trust chain file")
				}
				continue
			}
			chain = append(chain, base64.StdEncoding.EncodeToString(block.Bytes))
		}
	}
	client := authHTTPClient()
	client.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}}
	token, _ := json.Marshal(chain)
	return func(context.Context) (string, error) { return string(token), nil }, client, nil
}

// AWS access key set for signing the GetCallerIdentity subject token
type awsKeys struct{ accessKeyID, secretAccessKey, sessionToken string }

// Hex SHA256 of the data
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// HMAC SHA256 of the data under the key
func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// Percent encoding of every byte outside the RFC 3986 unreserved set
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// SigV4 Authorization header of a bodiless request over its host, date, token and extra headers
func awsSignedHeaders(method string, target *url.URL, keys awsKeys, region string, extra map[string]string, now time.Time) map[string]string {
	amzDate, day := now.UTC().Format("20060102T150405Z"), now.UTC().Format("20060102")
	headers := map[string]string{"host": target.Host, "x-amz-date": amzDate}
	if keys.sessionToken != "" {
		headers["x-amz-security-token"] = keys.sessionToken
	}
	for k, v := range extra {
		headers[strings.ToLower(k)] = v
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n + ":" + strings.Join(strings.Fields(headers[n]), " ") + "\n")
	}
	query := target.Query()
	qnames := make([]string, 0, len(query))
	for k := range query {
		qnames = append(qnames, k)
	}
	sort.Strings(qnames)
	var qparts []string
	for _, k := range qnames {
		values := append([]string(nil), query[k]...)
		sort.Strings(values)
		for _, v := range values {
			qparts = append(qparts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	path := target.EscapedPath()
	if path == "" {
		path = "/"
	}
	signed := strings.Join(names, ";")
	canonical := strings.Join([]string{method, path, strings.Join(qparts, "&"), canonHeaders.String(), signed, sha256Hex(nil)}, "\n")
	scope := day + "/" + region + "/sts/aws4_request"
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonical))}, "\n")
	key := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+keys.secretAccessKey), day), region), "sts"), "aws4_request")
	headers["authorization"] = "AWS4-HMAC-SHA256 Credential=" + keys.accessKeyID + "/" + scope + ", SignedHeaders=" + signed +
		", Signature=" + hex.EncodeToString(hmacSHA256(key, toSign))
	return headers
}

// AWS GetCallerIdentity request signed with environment or EC2 metadata keys, serialized as the subject token
func awsSubjectToken(src map[string]any, audience string) (subjectTokenSupplier, error) {
	m := awsEnvironmentID.FindStringSubmatch(str(src, "environment_id"))
	if m == nil {
		return nil, errors.New("no valid AWS credential_source provided")
	}
	if m[1] != "1" {
		return nil, fmt.Errorf("aws credential_source version %s is not supported", m[1])
	}
	regionURL, credsURL, sessionURL := str(src, "region_url"), str(src, "url"), str(src, "imdsv2_session_token_url")
	verifyURL := str(src, "regional_cred_verification_url")
	if verifyURL == "" {
		verifyURL = awsDefaultVerifyURL
	}
	return func(ctx context.Context) (string, error) {
		client := authHTTPClient()
		fetch := func(method, target string, headers map[string]string) (string, int, error) {
			req, err := http.NewRequestWithContext(ctx, method, target, nil)
			if err != nil {
				return "", 0, err
			}
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			resp, err := client.Do(req)
			if err != nil {
				return "", 0, err
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			return string(raw), resp.StatusCode, nil
		}
		region := os.Getenv("AWS_REGION")
		if region == "" {
			region = os.Getenv("AWS_DEFAULT_REGION")
		}
		keys := awsKeys{os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), os.Getenv("AWS_SESSION_TOKEN")}
		envKeys := keys.accessKeyID != "" && keys.secretAccessKey != ""
		meta := map[string]string{}
		if sessionURL != "" && (region == "" || !envKeys) {
			body, status, err := fetch(http.MethodPut, sessionURL, map[string]string{"X-aws-ec2-metadata-token-ttl-seconds": "300"})
			if err != nil || status != http.StatusOK {
				return "", fmt.Errorf("unable to retrieve AWS session token: %s %v", body, err)
			}
			meta["X-aws-ec2-metadata-token"] = body
		}
		if region == "" {
			if regionURL == "" {
				return "", errors.New("unable to determine AWS region")
			}
			body, status, err := fetch(http.MethodGet, regionURL, meta)
			if err != nil || status != http.StatusOK || body == "" {
				return "", fmt.Errorf("unable to retrieve AWS region: %s %v", body, err)
			}
			region = body[:len(body)-1]
		}
		if !envKeys {
			if credsURL == "" {
				return "", errors.New("unable to determine the AWS metadata security credentials endpoint")
			}
			role, status, err := fetch(http.MethodGet, credsURL, meta)
			if err != nil || status != http.StatusOK {
				return "", fmt.Errorf("unable to retrieve AWS role name: %s %v", role, err)
			}
			withJSON := map[string]string{"Content-Type": "application/json"}
			for k, v := range meta {
				withJSON[k] = v
			}
			body, status, err := fetch(http.MethodGet, credsURL+"/"+role, withJSON)
			var out struct{ AccessKeyId, SecretAccessKey, Token string }
			if err != nil || status != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil {
				return "", fmt.Errorf("unable to retrieve AWS security credentials: %s %v", body, err)
			}
			keys = awsKeys{out.AccessKeyId, out.SecretAccessKey, out.Token}
		}
		target := strings.ReplaceAll(verifyURL, "{region}", region)
		parsed, err := url.Parse(target)
		if err != nil {
			return "", err
		}
		signed := awsSignedHeaders(http.MethodPost, parsed, keys, region, map[string]string{"x-goog-cloud-target-resource": audience}, time.Now())
		headers := []map[string]string{{"key": "Authorization", "value": signed["authorization"]},
			{"key": "host", "value": parsed.Host}, {"key": "x-amz-date", "value": signed["x-amz-date"]}}
		if keys.sessionToken != "" {
			headers = append(headers, map[string]string{"key": "x-amz-security-token", "value": keys.sessionToken})
		}
		headers = append(headers, map[string]string{"key": "x-goog-cloud-target-resource", "value": audience})
		out, _ := json.Marshal(map[string]any{"headers": headers, "method": "POST", "url": target})
		return url.QueryEscape(string(out)), nil
	}, nil
}

// Pluggable auth subject token from an allowed executable, reusing an unexpired cached output file
func executableSubjectToken(src map[string]any, audience, subjectType, impURL string) (subjectTokenSupplier, error) {
	exe, _ := src["executable"].(map[string]any)
	command := str(exe, "command")
	if command == "" {
		return nil, errors.New("executable credential_source needs a command")
	}
	timeout := 30 * time.Second
	if ms, ok := exe["timeout_millis"].(float64); ok {
		if ms < 5000 || ms > 120000 {
			return nil, errors.New("executable timeout must be between 5 and 120 seconds")
		}
		timeout = time.Duration(ms) * time.Millisecond
	}
	outputFile := str(exe, "output_file")
	return func(ctx context.Context) (string, error) {
		if os.Getenv("GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES") != "1" {
			return "", errors.New("pluggable auth executables run only with GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES=1")
		}
		if outputFile != "" {
			if raw, err := os.ReadFile(outputFile); err == nil {
				var cached map[string]any
				if json.Unmarshal(raw, &cached) == nil {
					token, rerun, err := executableResponseToken(cached)
					switch {
					case err == nil && cached["expiration_time"] != nil:
						return token, nil
					case err != nil && !rerun:
						return "", err
					}
				}
			}
		}
		argv := strings.Fields(command)
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
		env := append(os.Environ(), "GOOGLE_EXTERNAL_ACCOUNT_AUDIENCE="+audience, "GOOGLE_EXTERNAL_ACCOUNT_TOKEN_TYPE="+subjectType,
			"GOOGLE_EXTERNAL_ACCOUNT_INTERACTIVE=0")
		if target, err := impersonationTarget(impURL); impURL != "" && err == nil {
			env = append(env, "GOOGLE_EXTERNAL_ACCOUNT_IMPERSONATED_EMAIL="+target)
		}
		if outputFile != "" {
			env = append(env, "GOOGLE_EXTERNAL_ACCOUNT_OUTPUT_FILE="+outputFile)
		}
		cmd.Env = env
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && runCtx.Err() == nil {
				return "", fmt.Errorf("executable exited with code %d: %s", exitErr.ExitCode(), strings.TrimSpace(out.String()))
			}
			return "", err
		}
		var response map[string]any
		if err := json.Unmarshal(out.Bytes(), &response); err != nil {
			return "", fmt.Errorf("unable to parse the executable response: %s", strings.TrimSpace(out.String()))
		}
		token, _, err := executableResponseToken(response)
		if err != nil {
			return "", err
		}
		if outputFile != "" && response["expiration_time"] == nil {
			return "", errors.New("executable response needs expiration_time when an output_file is configured")
		}
		return token, nil
	}, nil
}

// Subject token of a version 1 executable response, rerun true when running the executable again may succeed
func executableResponseToken(resp map[string]any) (string, bool, error) {
	version, ok := resp["version"].(float64)
	if !ok {
		return "", false, errors.New("executable response has no version")
	}
	if version > 1 {
		return "", true, fmt.Errorf("executable returned unsupported version %v", version)
	}
	success, ok := resp["success"].(bool)
	if !ok {
		return "", false, errors.New("executable response has no success field")
	}
	if !success {
		code, okc := resp["code"]
		msg, okm := resp["message"]
		if !okc || !okm {
			return "", false, errors.New("executable failure response needs code and message")
		}
		return "", true, fmt.Errorf("executable returned unsuccessful response, code %v, message %v", code, msg)
	}
	if exp, ok := resp["expiration_time"].(float64); ok && exp < float64(time.Now().Unix()) {
		return "", true, errors.New("the token returned by the executable is expired")
	}
	tokenType, ok := resp["token_type"].(string)
	if !ok {
		return "", false, errors.New("executable response has no token_type")
	}
	switch tokenType {
	case "urn:ietf:params:oauth:token-type:jwt", "urn:ietf:params:oauth:token-type:id_token":
		return str(resp, "id_token"), false, nil
	case "urn:ietf:params:oauth:token-type:saml2":
		return str(resp, "saml_response"), false, nil
	}
	return "", true, errors.New("executable returned an unsupported token type")
}

// Workforce identity end user credentials refreshed at token_url with basic client authentication
func externalAuthorizedUserCredentials(info map[string]any) (*googleCredentials, error) {
	refresh, tokenURL := str(info, "refresh_token"), str(info, "token_url")
	clientID, clientSecret := str(info, "client_id"), str(info, "client_secret")
	if refresh == "" || tokenURL == "" || clientID == "" || clientSecret == "" {
		return nil, errors.New("external authorized user info needs refresh_token, token_url, client_id and client_secret")
	}
	c := &googleCredentials{kind: "external_account_authorized_user", quotaProject: str(info, "quota_project_id")}
	c.mint = func(ctx context.Context) (string, time.Time, error) {
		now := time.Now().UTC()
		data, err := postSTS(ctx, tokenURL, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}, clientID, clientSecret, nil)
		if err != nil {
			return "", time.Time{}, err
		}
		if rt := str(data, "refresh_token"); rt != "" {
			refresh = rt
		}
		return tokenFrom(data, now)
	}
	return c, nil
}
