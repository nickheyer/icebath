package gcp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth/aws"
	"github.com/nickheyer/icebath/internal/proc"
)

const (
	awsSessionTTL        = "300"
	executableDefault    = 30 * time.Second
	executableLowerBound = 5 * time.Second
	executableUpperBound = 120 * time.Second
	executableOutput     = 1 << 20
)

var awsEnvironment = regexp.MustCompile(`^(aws)(\d+)$`)

type awsSource struct {
	c               *client
	regionURL       string
	credentialsURL  string
	sessionTokenURL string
	verificationURL string
	audience        string

	mu     sync.Mutex
	region string
	known  bool
}

func awsFailure(message string, status int, err error) *Error {
	return &Error{Kind: kindExternal, Message: message, Status: status, Err: err}
}

func validMetadataURL(raw string) bool {
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Hostname() == "169.254.169.254" || u.Hostname() == "fd00:ec2::254"
}

func (c *client) awsSubject(source document, audience string) (subjectSupplier, error) {
	s := &awsSource{
		c: c, audience: audience,
		regionURL: source.text("region_url"), credentialsURL: source.text("url"),
		sessionTokenURL: source.text("imdsv2_session_token_url"), verificationURL: source.text("regional_cred_verification_url"),
	}
	match := awsEnvironment.FindStringSubmatch(source.text("environment_id"))
	if match == nil || s.verificationURL == "" {
		return nil, awsFailure("no valid AWS credential_source provided", 0, nil)
	}
	if version, err := strconv.Atoi(match[2]); err != nil || version != 1 {
		return nil, awsFailure("aws version "+match[2]+" is not supported", 0, nil)
	}
	for name, value := range map[string]string{"region_url": s.regionURL, "url": s.credentialsURL, "imdsv2_session_token_url": s.sessionTokenURL} {
		if !validMetadataURL(value) {
			return nil, awsFailure("invalid hostname in credential_source field "+name, 0, nil)
		}
	}
	return s.subject, nil
}

func (s *awsSource) metadata(ctx context.Context, method, target string, header http.Header) (int, string, error) {
	status, _, data, err := s.c.send(ctx, s.c.api, requestTimeout, method, target, header, nil)
	if err != nil {
		return 0, "", awsFailure("AWS metadata server could not be reached: "+httpclient.Message(err), 0, err)
	}
	return status, string(data), nil
}

func (s *awsSource) sessionHeader(ctx context.Context) (http.Header, error) {
	header := http.Header{}
	if s.sessionTokenURL == "" {
		return header, nil
	}
	status, token, err := s.metadata(ctx, http.MethodPut, s.sessionTokenURL, http.Header{"X-aws-ec2-metadata-token-ttl-seconds": {awsSessionTTL}})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, awsFailure("unable to retrieve the AWS session token", status, nil)
	}
	header["X-aws-ec2-metadata-token"] = []string{token}
	return header, nil
}

func (s *awsSource) resolveRegion(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.known {
		return s.region, nil
	}
	for _, name := range []string{"AWS_REGION", "AWS_DEFAULT_REGION"} {
		if value, ok := os.LookupEnv(name); ok {
			s.region, s.known = value, true
			return value, nil
		}
	}
	if s.regionURL == "" {
		return "", awsFailure("unable to determine the AWS region", 0, nil)
	}
	header, err := s.sessionHeader(ctx)
	if err != nil {
		return "", err
	}
	status, zone, err := s.metadata(ctx, http.MethodGet, s.regionURL, header)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", awsFailure("unable to retrieve the AWS region", status, nil)
	}
	if zone != "" {
		zone = zone[:len(zone)-1]
	}
	s.region, s.known = zone, true
	return zone, nil
}

func (s *awsSource) securityCredentials(ctx context.Context) (aws.Credentials, error) {
	keyID, secret := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
	if keyID != "" && secret != "" {
		return aws.Credentials{AccessKeyID: keyID, SecretAccessKey: secret, SessionToken: os.Getenv("AWS_SESSION_TOKEN"), Source: "env"}, nil
	}
	header, err := s.sessionHeader(ctx)
	if err != nil {
		return aws.Credentials{}, err
	}
	if s.credentialsURL == "" {
		return aws.Credentials{}, awsFailure("unable to determine the AWS metadata server security credentials endpoint", 0, nil)
	}
	status, role, err := s.metadata(ctx, http.MethodGet, s.credentialsURL, header)
	if err != nil {
		return aws.Credentials{}, err
	}
	if status != http.StatusOK {
		return aws.Credentials{}, awsFailure("unable to retrieve the AWS role name", status, nil)
	}
	header = header.Clone()
	header["Content-Type"] = []string{jsonType}
	status, body, err := s.metadata(ctx, http.MethodGet, s.credentialsURL+"/"+role, header)
	if err != nil {
		return aws.Credentials{}, err
	}
	if status != http.StatusOK {
		return aws.Credentials{}, awsFailure("unable to retrieve AWS security credentials", status, nil)
	}
	var reply struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		Token           string `json:"Token"`
	}
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		return aws.Credentials{}, awsFailure("AWS security credentials reply is not a JSON object", 0, nil)
	}
	return aws.Credentials{AccessKeyID: reply.AccessKeyID, SecretAccessKey: reply.SecretAccessKey, SessionToken: reply.Token, Source: "iam-role"}, nil
}

func awsQuote(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(quotePath(s), "/", "%2F"), "+", "%2B")
}

func canonicalQuery(raw string) string {
	values, _ := url.ParseQuery(raw)
	encoded := map[string][]string{}
	var keys []string
	for key, items := range values {
		name := awsQuote(key)
		for _, item := range items {
			if item == "" {
				continue
			}
			if _, seen := encoded[name]; !seen {
				keys = append(keys, name)
			}
			encoded[name] = append(encoded[name], awsQuote(item))
		}
	}
	sort.Strings(keys)
	var pairs []string
	for _, name := range keys {
		sort.Strings(encoded[name])
		for _, item := range encoded[name] {
			pairs = append(pairs, name+"="+item)
		}
	}
	return strings.Join(pairs, "&")
}

func hmacSHA256(key []byte, message string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}

func (s *awsSource) subject(ctx context.Context) (string, error) {
	region, err := s.resolveRegion(ctx)
	if err != nil {
		return "", err
	}
	creds, err := s.securityCredentials(ctx)
	if err != nil {
		return "", err
	}
	target := strings.ReplaceAll(s.verificationURL, "{region}", region)
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" || u.Scheme != "https" {
		return "", awsFailure("invalid AWS service URL", 0, nil)
	}
	host := u.Hostname()
	service, _, _ := strings.Cut(host, ".")
	canonicalURI := "/"
	if u.Path != "" {
		canonicalURI = path.Clean(u.Path)
	}
	now := time.Now().UTC()
	amzDate, dateStamp := now.Format("20060102T150405Z"), now.Format("20060102")
	headers := map[string]string{"host": host, "x-amz-date": amzDate}
	if creds.SessionToken != "" {
		headers["x-amz-security-token"] = creds.SessionToken
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name + ":" + headers[name] + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	emptyHash := sha256.Sum256(nil)
	canonicalRequest := strings.Join([]string{http.MethodPost, canonicalURI, canonicalQuery(u.RawQuery), canonicalHeaders.String(), signedHeaders, hex.EncodeToString(emptyHash[:])}, "\n")
	requestHash := sha256.Sum256([]byte(canonicalRequest))
	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(requestHash[:])}, "\n")
	key := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+creds.SecretAccessKey), dateStamp), region), service), "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))
	headers["Authorization"] = "AWS4-HMAC-SHA256 Credential=" + creds.AccessKeyID + "/" + scope + ", SignedHeaders=" + signedHeaders + ", Signature=" + signature
	headers["x-goog-cloud-target-resource"] = s.audience
	names = names[:0]
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	list := make([]object, 0, len(names))
	for _, name := range names {
		list = append(list, object{{"key", name}, {"value", headers[name]}})
	}
	return quotePath(string(compactJSON(object{{"headers", list}, {"method", http.MethodPost}, {"url", target}}))), nil
}

type executableSource struct {
	command    []string
	timeout    time.Duration
	outputFile string
	env        []string
}

func executableFailure(message string) *Error { return &Error{Kind: kindExternal, Message: message} }

func (c *client) executableSubject(source document, audience, subjectType, impersonationURL, principal string) (subjectSupplier, error) {
	spec, ok := source.object("executable")
	if !ok || len(spec) == 0 {
		return nil, executableFailure("missing credential_source, an executable must be provided")
	}
	s := &executableSource{command: strings.Fields(spec.text("command")), timeout: executableDefault, outputFile: spec.text("output_file")}
	if len(s.command) == 0 {
		return nil, executableFailure("missing command field, executable command must be provided")
	}
	if millis, ok := spec.number("timeout_millis"); ok && millis != 0 {
		s.timeout = time.Duration(millis * float64(time.Millisecond))
		if s.timeout < executableLowerBound || s.timeout > executableUpperBound {
			return nil, executableFailure("timeout must be between 5 and 120 seconds")
		}
	}
	s.env = []string{
		"GOOGLE_EXTERNAL_ACCOUNT_AUDIENCE=" + audience,
		"GOOGLE_EXTERNAL_ACCOUNT_TOKEN_TYPE=" + subjectType,
		"GOOGLE_EXTERNAL_ACCOUNT_ID=" + principal,
		"GOOGLE_EXTERNAL_ACCOUNT_INTERACTIVE=0",
	}
	if impersonationURL != "" {
		s.env = append(s.env, "GOOGLE_EXTERNAL_ACCOUNT_IMPERSONATED_EMAIL="+principal)
	}
	if spec.has("output_file") && string(spec["output_file"]) != "null" {
		s.env = append(s.env, "GOOGLE_EXTERNAL_ACCOUNT_OUTPUT_FILE="+s.outputFile)
	}
	return s.subject, nil
}

func executableToken(reply document) (token string, usable bool, err error) {
	if !reply.has("version") {
		return "", false, executableFailure("the executable response is missing the version field")
	}
	if version, ok := reply.number("version"); !ok || version > 1 {
		return "", true, executableFailure("executable returned an unsupported version")
	}
	if !reply.has("success") {
		return "", false, executableFailure("the executable response is missing the success field")
	}
	var success bool
	_ = json.Unmarshal(reply["success"], &success)
	if !success {
		if !reply.has("code") || !reply.has("message") {
			return "", false, executableFailure("error code and message fields are required in the response")
		}
		return "", true, &Error{Kind: kindExternal, Code: reply.text("code"), Message: "executable returned an unsuccessful response: " + reply.text("message")}
	}
	if expiry, ok := reply.number("expiration_time"); ok && expiry < float64(time.Now().Unix()) {
		return "", true, executableFailure("the token returned by the executable is expired")
	}
	if !reply.has("token_type") {
		return "", false, executableFailure("the executable response is missing the token_type field")
	}
	switch reply.text("token_type") {
	case "urn:ietf:params:oauth:token-type:jwt", "urn:ietf:params:oauth:token-type:id_token":
		return reply.text("id_token"), true, nil
	case "urn:ietf:params:oauth:token-type:saml2":
		return reply.text("saml_response"), true, nil
	}
	return "", true, executableFailure("executable returned an unsupported token type")
}

type cappedBuffer struct {
	buf bytes.Buffer
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	if room := executableOutput - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}

func (s *executableSource) subject(ctx context.Context) (string, error) {
	if os.Getenv("GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES") != "1" {
		return "", executableFailure("executables need to be explicitly allowed (set GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES to 1) to run")
	}
	if s.outputFile != "" {
		if data, err := readBounded(s.outputFile); err == nil {
			if cached, ok := parseDocument(data); ok {
				token, expired, err := executableToken(cached)
				switch {
				case err == nil && cached.has("expiration_time"):
					return token, nil
				case err != nil && !expired:
					return "", err
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	cmd := exec.Command(s.command[0], s.command[1:]...)
	cmd.Env = append(os.Environ(), s.env...)
	var out cappedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	proc.Hide(cmd)
	res, err := proc.Run(ctx, cmd)
	if err != nil {
		if ctx.Err() != nil {
			return "", &Error{Kind: kindExternal, Message: "executable did not finish within " + s.timeout.String(), Err: ctx.Err()}
		}
		return "", &Error{Kind: kindExternal, Message: "executable could not be run", Err: err}
	}
	if !res.Exit.Success() {
		return "", executableFailure("executable exited with a non-zero return code (" + res.Exit.String() + ")")
	}
	var reply document
	if out.buf.Len() > 0 {
		parsed, ok := parseDocument(out.buf.Bytes())
		if !ok {
			return "", executableFailure("the executable response is not a JSON object")
		}
		reply = parsed
	}
	if len(reply) == 0 && s.outputFile != "" {
		data, err := readBounded(s.outputFile)
		if err != nil {
			return "", &Error{Kind: kindExternal, Message: "executable output file " + s.outputFile + " could not be read", Err: err}
		}
		parsed, ok := parseDocument(data)
		if !ok {
			return "", executableFailure("executable output file " + s.outputFile + " is not a JSON object")
		}
		reply = parsed
	}
	if reply == nil {
		return "", executableFailure("the executable printed no response")
	}
	token, _, err := executableToken(reply)
	return token, err
}
