package provider

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
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Source of AWS credentials, refreshed by the implementation as they near expiry
type awsCredentialProvider interface {
	Retrieve(ctx context.Context) (awsCredentials, error)
}

// Fixed credentials that never refresh
type awsStaticProvider struct{ creds awsCredentials }

func (p awsStaticProvider) Retrieve(context.Context) (awsCredentials, error) {
	if !p.creds.valid() {
		return awsCredentials{}, &AWSCredentialError{Provider: p.creds.Source, Message: "static credentials are missing the access key id or secret access key"}
	}
	return p.creds, nil
}

// AWSCredentialError is a credential source that applied but could not produce credentials
type AWSCredentialError struct {
	Provider string
	Message  string
	Err      error
}

func (e *AWSCredentialError) Error() string {
	if e.Provider != "" {
		return fmt.Sprintf("Error when retrieving credentials from %s: %s", e.Provider, e.Message)
	}
	return e.Message
}

func (e *AWSCredentialError) Unwrap() error { return e.Err }

// ErrNoAWSCredentials is the chain walked to the end without any source answering
var ErrNoAWSCredentials = errors.New("Unable to locate credentials")

// Botocore advisory and mandatory refresh windows ahead of credential expiry
const (
	awsAdvisoryRefresh  = 15 * time.Minute
	awsMandatoryRefresh = 10 * time.Minute
)

// Clock the credential cache and token refresh consult, replaced by tests
var awsNow = time.Now

// Session inputs mirroring the boto3 Session keyword arguments
type awsSessionOptions struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Profile         string
	DisableIMDS     bool
}

// Resolved session view over the environment and the shared config files
type awsSession struct {
	opts     awsSessionOptions
	lookup   func(string) (string, bool)
	http     *http.Client
	loadOnce sync.Once
	cfg      *awsConfigFiles
	cfgErr   error
}

func newAWSSession(opts awsSessionOptions) *awsSession {
	return &awsSession{opts: opts, lookup: os.LookupEnv, http: &http.Client{}}
}

func (s *awsSession) env(name string) string {
	v, _ := s.lookup(name)
	return v
}

func (s *awsSession) hasEnv(name string) bool {
	_, ok := s.lookup(name)
	return ok
}

// Profile in effect, explicit option then AWS_DEFAULT_PROFILE then AWS_PROFILE then default
func (s *awsSession) profileName() string {
	if s.opts.Profile != "" {
		return s.opts.Profile
	}
	for _, name := range []string{"AWS_DEFAULT_PROFILE", "AWS_PROFILE"} {
		if v := s.env(name); v != "" {
			return v
		}
	}
	return "default"
}

func (s *awsSession) configFiles() (*awsConfigFiles, error) {
	s.loadOnce.Do(func() {
		s.cfg, s.cfgErr = loadAWSConfigFiles(s.env("AWS_CONFIG_FILE"), s.env("AWS_SHARED_CREDENTIALS_FILE"))
	})
	return s.cfg, s.cfgErr
}

// Merged profile values, an unknown named profile is an error and an absent default is empty
func (s *awsSession) profile(name string) (map[string]string, error) {
	cfg, err := s.configFiles()
	if err != nil {
		return nil, err
	}
	if p, ok := cfg.merged[name]; ok {
		return p, nil
	}
	if name == "default" {
		return map[string]string{}, nil
	}
	return nil, &AWSCredentialError{Message: fmt.Sprintf("The config profile (%s) could not be found", name)}
}

// Value of a setting from its environment variable, else from the active profile
func (s *awsSession) setting(envName, profileKey string) string {
	if envName != "" {
		if v, ok := s.lookup(envName); ok {
			return v
		}
	}
	p, err := s.profile(s.profileName())
	if err != nil {
		return ""
	}
	return p[profileKey]
}

// Region from AWS_DEFAULT_REGION then the active profile, empty when neither sets one
func (s *awsSession) region() string {
	return strings.TrimSpace(s.setting("AWS_DEFAULT_REGION", "region"))
}

// Botocore boolean coercion of a config string
func awsEnsureBool(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), "true")
}

// Configured endpoint for a service, service env then global env then services section then profile endpoint_url
func (s *awsSession) configuredEndpoint(serviceID string) string {
	snake := strings.ReplaceAll(strings.ToLower(serviceID), "-", "_")
	if v, ok := s.lookup("AWS_IGNORE_CONFIGURED_ENDPOINT_URLS"); ok {
		if awsEnsureBool(v) {
			return ""
		}
	} else if p, err := s.profile(s.profileName()); err == nil && awsEnsureBool(p["ignore_configured_endpoint_urls"]) {
		return ""
	}
	if v := s.env("AWS_ENDPOINT_URL_" + strings.ToUpper(snake)); v != "" {
		return v
	}
	if v := s.env("AWS_ENDPOINT_URL"); v != "" {
		return v
	}
	p, err := s.profile(s.profileName())
	if err != nil {
		return ""
	}
	if section := p["services"]; section != "" {
		if cfg, err := s.configFiles(); err == nil {
			if svc, ok := cfg.services[section][snake]; ok && svc["endpoint_url"] != "" {
				return svc["endpoint_url"]
			}
		}
	}
	return p["endpoint_url"]
}

// DNS suffix of the partition a region belongs to
func awsPartitionDNSSuffix(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "amazonaws.com.cn"
	case strings.HasPrefix(region, "us-isob-"):
		return "sc2s.sgov.gov"
	case strings.HasPrefix(region, "us-isof-"):
		return "csp.hci.ic.gov"
	case strings.HasPrefix(region, "us-iso-"):
		return "c2s.ic.gov"
	case strings.HasPrefix(region, "eu-isoe-"):
		return "cloud.adc-e.uk"
	}
	return "amazonaws.com"
}

// ---- shared config files ----

// Shared config and credentials files parsed the way botocore loads them
type awsConfigFiles struct {
	profiles    map[string]map[string]string
	credentials map[string]map[string]string
	merged      map[string]map[string]string
	ssoSessions map[string]map[string]string
	services    map[string]map[string]map[string]string
}

// Expands environment references and a leading tilde in a configured path
func awsExpandPath(path string) string {
	path = os.ExpandEnv(path)
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, path[1:])
		}
	}
	return path
}

func awsDefaultFile(override, name string) string {
	if override != "" {
		return awsExpandPath(override)
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".aws", name)
}

// Reads both shared files into profile, sso-session and services maps, a missing file reads as empty
func loadAWSConfigFiles(configOverride, credentialsOverride string) (*awsConfigFiles, error) {
	out := &awsConfigFiles{profiles: map[string]map[string]string{}, credentials: map[string]map[string]string{},
		merged: map[string]map[string]string{}, ssoSessions: map[string]map[string]string{}, services: map[string]map[string]map[string]string{}}
	cfgDoc, err := parseAWSINIFile(awsDefaultFile(configOverride, "config"))
	if err != nil {
		return nil, err
	}
	for _, sec := range cfgDoc.order {
		values := cfgDoc.flat[sec]
		switch {
		case strings.HasPrefix(sec, "profile"):
			if parts, err := awsPOSIXSplit(sec); err == nil && len(parts) == 2 {
				out.profiles[parts[1]] = values
			}
		case strings.HasPrefix(sec, "sso-session"):
			if parts, err := awsPOSIXSplit(sec); err == nil && len(parts) == 2 {
				out.ssoSessions[parts[1]] = values
			}
		case strings.HasPrefix(sec, "services"):
			if parts, err := awsPOSIXSplit(sec); err == nil && len(parts) == 2 {
				out.services[parts[1]] = cfgDoc.nested[sec]
			}
		case sec == "default":
			out.profiles["default"] = values
		}
	}
	credDoc, err := parseAWSINIFile(awsDefaultFile(credentialsOverride, "credentials"))
	if err != nil {
		return nil, err
	}
	for _, sec := range credDoc.order {
		out.credentials[sec] = credDoc.flat[sec]
	}
	for name, p := range out.profiles {
		out.merged[name] = copyStringMap(p)
	}
	for name, p := range out.credentials {
		m, ok := out.merged[name]
		if !ok {
			m = map[string]string{}
			out.merged[name] = m
		}
		for k, v := range p {
			m[k] = v
		}
	}
	return out, nil
}

// One parsed INI document, flat values plus the single nesting level botocore supports
type awsINIDoc struct {
	order  []string
	flat   map[string]map[string]string
	nested map[string]map[string]map[string]string
}

// Parses a file with configparser rules, a missing file is an empty document
func parseAWSINIFile(path string) (*awsINIDoc, error) {
	empty := &awsINIDoc{flat: map[string]map[string]string{}, nested: map[string]map[string]map[string]string{}}
	if path == "" {
		return empty, nil
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return empty, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &AWSCredentialError{Message: fmt.Sprintf("Unable to parse config file: %s", path), Err: err}
	}
	doc, err := parseAWSINI(string(data))
	if err != nil {
		return nil, &AWSCredentialError{Message: fmt.Sprintf("Unable to parse config file: %s", path), Err: err}
	}
	return doc, nil
}

var awsINIOption = regexp.MustCompile(`^(.*?)\s*([=:])\s*(.*)$`)

// RawConfigParser semantics, strict duplicates, DEFAULT inheritance and indented continuation lines
func parseAWSINI(text string) (*awsINIDoc, error) {
	type rawSection struct {
		keys   []string
		values map[string][]string
	}
	sections := map[string]*rawSection{}
	var order []string
	defaults := &rawSection{values: map[string][]string{}}
	var cur *rawSection
	curOpt := ""
	indent := 0
	var errs []string
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for lineno, line := range lines {
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "#") || strings.HasPrefix(stripped, ";") {
			continue
		}
		if stripped == "" {
			if cur != nil && curOpt != "" {
				cur.values[curOpt] = append(cur.values[curOpt], "")
			}
			continue
		}
		lead := len(line) - len(strings.TrimLeft(line, " \t"))
		if cur != nil && curOpt != "" && lead > indent {
			cur.values[curOpt] = append(cur.values[curOpt], stripped)
			continue
		}
		indent = lead
		if strings.HasPrefix(stripped, "[") {
			if end := strings.LastIndex(stripped, "]"); end > 1 {
				name := stripped[1:end]
				curOpt = ""
				if name == "DEFAULT" {
					cur = defaults
					continue
				}
				if _, dup := sections[name]; dup {
					return nil, fmt.Errorf("While reading config: section %q already exists", name)
				}
				cur = &rawSection{values: map[string][]string{}}
				sections[name] = cur
				order = append(order, name)
				continue
			}
		}
		if cur == nil {
			return nil, fmt.Errorf("File contains no section headers. line %d: %q", lineno+1, line)
		}
		m := awsINIOption.FindStringSubmatch(stripped)
		if m == nil || strings.TrimSpace(m[1]) == "" {
			errs = append(errs, fmt.Sprintf("[line %2d]: %q", lineno+1, line))
			curOpt = ""
			continue
		}
		opt := strings.ToLower(strings.TrimRight(m[1], " \t"))
		if _, dup := cur.values[opt]; dup {
			return nil, fmt.Errorf("While reading config: option %q in section already exists", opt)
		}
		cur.keys = append(cur.keys, opt)
		cur.values[opt] = []string{strings.TrimSpace(m[3])}
		curOpt = opt
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("Source contains parsing errors: %s", strings.Join(errs, " "))
	}
	doc := &awsINIDoc{order: order, flat: map[string]map[string]string{}, nested: map[string]map[string]map[string]string{}}
	join := func(parts []string) string { return strings.TrimRight(strings.Join(parts, "\n"), " \t\n") }
	for _, name := range order {
		sec := sections[name]
		flat := map[string]string{}
		nested := map[string]map[string]string{}
		merged := map[string][]string{}
		for k, v := range defaults.values {
			merged[k] = v
		}
		for k, v := range sec.values {
			merged[k] = v
		}
		for k, v := range merged {
			val := join(v)
			if len(v) > 1 && v[0] == "" {
				sub, err := awsParseNested(val)
				if err != nil {
					return nil, err
				}
				nested[k] = sub
			}
			flat[k] = val
		}
		doc.flat[name] = flat
		doc.nested[name] = nested
	}
	return doc, nil
}

// Indented key value lines of one nested option
func awsParseNested(value string) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("not enough values to unpack in nested config line %q", line)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// POSIX shlex split with comments off, the rules botocore applies to section names and commands
func awsPOSIXSplit(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			if i+1 >= len(s) {
				return nil, errors.New("No escaped character")
			}
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("No closing quotation")
			}
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case c == '"':
			i++
			closed := false
			for ; i < len(s); i++ {
				if s[i] == '"' {
					closed = true
					break
				}
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					i++
				}
				cur.WriteByte(s[i])
			}
			if !closed {
				return nil, errors.New("No closing quotation")
			}
			inWord = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// Windows command line split following the C runtime backslash and quote rules botocore mirrors
func awsWindowsSplit(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var components []string
	var buff []string
	quoted := false
	backslashes := 0
	flushSlashes := func() {
		if backslashes > 0 {
			buff = append(buff, strings.Repeat(`\`, backslashes))
			backslashes = 0
		}
	}
	for _, ch := range s {
		switch {
		case ch == '\\':
			backslashes++
		case ch == '"':
			if backslashes > 0 {
				buff = append(buff, strings.Repeat(`\`, backslashes/2))
				odd := backslashes%2 == 1
				backslashes = 0
				if odd {
					buff = append(buff, `"`)
					continue
				}
			}
			quoted = !quoted
			buff = append(buff, "")
		case (ch == ' ' || ch == '\t') && !quoted:
			flushSlashes()
			if len(buff) > 0 {
				components = append(components, strings.Join(buff, ""))
				buff = nil
			}
		default:
			flushSlashes()
			buff = append(buff, string(ch))
		}
	}
	if quoted {
		return nil, fmt.Errorf("No closing quotation in string: %s", s)
	}
	flushSlashes()
	if len(buff) > 0 {
		components = append(components, strings.Join(buff, ""))
	}
	return components, nil
}

// Platform shell split botocore uses for credential_process
func awsShellSplit(s string) ([]string, error) {
	if runtime.GOOS == "windows" {
		return awsWindowsSplit(s)
	}
	return awsPOSIXSplit(s)
}

// Parses the ISO 8601 timestamps AWS services and tools write
func awsParseTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if strings.HasSuffix(v, "UTC") {
		v = strings.TrimSuffix(v, "UTC") + "Z"
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05", "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable timestamp %q", v)
}

// ---- refreshing cache ----

// Fetches one fresh credential set from a source
type awsFetchFunc func(ctx context.Context) (awsCredentials, error)

// Credentials cached until the advisory window, refreshed ahead of expiry with the botocore windows
type awsRefreshingCredentials struct {
	mu    sync.Mutex
	fetch awsFetchFunc
	creds awsCredentials
	have  bool
}

func newAWSRefreshing(fetch awsFetchFunc, initial *awsCredentials) *awsRefreshingCredentials {
	r := &awsRefreshingCredentials{fetch: fetch}
	if initial != nil {
		r.creds, r.have = *initial, true
	}
	return r
}

// Retrieve returns cached credentials, refreshing inside the advisory window and failing inside the mandatory one
func (r *awsRefreshingCredentials) Retrieve(ctx context.Context) (awsCredentials, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := awsNow()
	if r.have && (r.creds.Expires.IsZero() || r.creds.Expires.Sub(now) > awsAdvisoryRefresh) {
		return r.creds, nil
	}
	fresh, err := r.fetch(ctx)
	if err == nil && !fresh.valid() {
		err = &AWSCredentialError{Provider: fresh.Source, Message: "refreshed credentials are missing the access key id or secret access key"}
	}
	if err != nil {
		if r.have && r.creds.Expires.Sub(now) > awsMandatoryRefresh {
			return r.creds, nil
		}
		return awsCredentials{}, err
	}
	r.creds, r.have = fresh, true
	if !fresh.Expires.IsZero() && !fresh.Expires.After(awsNow()) {
		return awsCredentials{}, &AWSCredentialError{Provider: fresh.Source, Message: "Credentials were refreshed, but the refreshed credentials are still expired."}
	}
	return r.creds, nil
}

// ---- default chain ----

// Credential provider walking the default chain once, then refreshing through the source that answered
type awsChainCredentials struct {
	session *awsSession
	mu      sync.Mutex
	source  awsCredentialProvider
}

func newAWSChainCredentials(opts awsSessionOptions) *awsChainCredentials {
	return &awsChainCredentials{session: newAWSSession(opts)}
}

// Retrieve resolves the answering source on first use and asks it for current credentials
func (c *awsChainCredentials) Retrieve(ctx context.Context) (awsCredentials, error) {
	c.mu.Lock()
	src := c.source
	c.mu.Unlock()
	if src == nil {
		resolved, err := resolveAWSCredentialSource(ctx, c.session)
		if err != nil {
			return awsCredentials{}, err
		}
		c.mu.Lock()
		if c.source == nil {
			c.source = resolved
		}
		src = c.source
		c.mu.Unlock()
	}
	return src.Retrieve(ctx)
}

// One link of the default chain, nil provider when the source does not apply
type awsChainLink struct {
	name string
	load func(ctx context.Context, s *awsSession) (awsCredentialProvider, error)
}

// Walks explicit keys, env, assume role, web identity, sso, shared credentials, process, config, container and IMDS
func resolveAWSCredentialSource(ctx context.Context, s *awsSession) (awsCredentialProvider, error) {
	if s.opts.AccessKeyID != "" || s.opts.SecretAccessKey != "" {
		creds := awsCredentials{AccessKeyID: s.opts.AccessKeyID, SecretAccessKey: s.opts.SecretAccessKey, SessionToken: s.opts.SessionToken, Source: "explicit"}
		if !creds.valid() {
			return nil, &AWSCredentialError{Provider: "explicit", Message: "Partial credentials found in explicit, missing: aws_access_key_id or aws_secret_access_key"}
		}
		return awsStaticProvider{creds}, nil
	}
	envEnabled := s.opts.Profile == ""
	profile := s.profileName()
	if _, err := s.profile(profile); err != nil {
		return nil, err
	}
	links := []awsChainLink{}
	if envEnabled {
		links = append(links, awsChainLink{"env", func(ctx context.Context, s *awsSession) (awsCredentialProvider, error) { return awsEnvProvider(s) }})
	}
	links = append(links,
		awsChainLink{"assume-role", func(ctx context.Context, s *awsSession) (awsCredentialProvider, error) {
			return newAWSAssumeRoleResolver(s).load(ctx, profile)
		}},
	)
	links = append(links, awsProfileLinks(profile, !envEnabled)...)
	links = append(links,
		awsChainLink{"container-role", func(ctx context.Context, s *awsSession) (awsCredentialProvider, error) {
			return awsContainerProvider(ctx, s)
		}},
		awsChainLink{"iam-role", func(ctx context.Context, s *awsSession) (awsCredentialProvider, error) {
			return awsIMDSProvider(ctx, s)
		}},
	)
	for _, link := range links {
		p, err := link.load(ctx, s)
		if err != nil {
			return nil, err
		}
		if p != nil {
			return p, nil
		}
	}
	return nil, ErrNoAWSCredentials
}

// Web identity, sso, shared credentials, process and config file sources of one profile
func awsProfileLinks(profile string, disableEnv bool) []awsChainLink {
	return []awsChainLink{
		{"assume-role-with-web-identity", func(ctx context.Context, s *awsSession) (awsCredentialProvider, error) {
			return awsWebIdentityProvider(s, profile, disableEnv)
		}},
		{"sso", func(ctx context.Context, s *awsSession) (awsCredentialProvider, error) {
			return awsSSOProvider(s, profile)
		}},
		{"shared-credentials-file", func(ctx context.Context, s *awsSession) (awsCredentialProvider, error) {
			return awsSharedCredentialsProvider(s, profile)
		}},
		{"custom-process", func(ctx context.Context, s *awsSession) (awsCredentialProvider, error) {
			return awsProcessProvider(s, profile)
		}},
		{"config-file", func(ctx context.Context, s *awsSession) (awsCredentialProvider, error) {
			return awsConfigFileProvider(s, profile)
		}},
	}
}

// Static or expiring keys from the AWS_ environment variables, partial keys are an error
func awsEnvProvider(s *awsSession) (awsCredentialProvider, error) {
	if s.env("AWS_ACCESS_KEY_ID") == "" {
		return nil, nil
	}
	read := func(context.Context) (awsCredentials, error) {
		creds := awsCredentials{AccessKeyID: s.env("AWS_ACCESS_KEY_ID"), SecretAccessKey: s.env("AWS_SECRET_ACCESS_KEY"), Source: "env"}
		if creds.AccessKeyID == "" {
			return awsCredentials{}, &AWSCredentialError{Provider: "env", Message: "Partial credentials found in env, missing: AWS_ACCESS_KEY_ID"}
		}
		if creds.SecretAccessKey == "" {
			return awsCredentials{}, &AWSCredentialError{Provider: "env", Message: "Partial credentials found in env, missing: AWS_SECRET_ACCESS_KEY"}
		}
		for _, name := range []string{"AWS_SECURITY_TOKEN", "AWS_SESSION_TOKEN"} {
			if v, ok := s.lookup(name); ok {
				creds.SessionToken = v
				break
			}
		}
		if exp := s.env("AWS_CREDENTIAL_EXPIRATION"); exp != "" {
			t, err := awsParseTime(exp)
			if err != nil {
				return awsCredentials{}, &AWSCredentialError{Provider: "env", Message: err.Error()}
			}
			creds.Expires = t
		}
		return creds, nil
	}
	creds, err := read(context.Background())
	if err != nil {
		return nil, err
	}
	if creds.Expires.IsZero() {
		return awsStaticProvider{creds}, nil
	}
	return newAWSRefreshing(read, &creds), nil
}

// Static keys of one section, nil when the access key is absent and an error when only half is set
func awsStaticFromSection(section map[string]string, provider string) (awsCredentialProvider, error) {
	key := section["aws_access_key_id"]
	if key == "" {
		return nil, nil
	}
	secret := section["aws_secret_access_key"]
	if secret == "" {
		return nil, &AWSCredentialError{Provider: provider, Message: "Partial credentials found in " + provider + ", missing: aws_secret_access_key"}
	}
	token := section["aws_session_token"]
	if token == "" {
		token = section["aws_security_token"]
	}
	return awsStaticProvider{awsCredentials{AccessKeyID: key, SecretAccessKey: secret, SessionToken: token, Source: provider}}, nil
}

func awsSharedCredentialsProvider(s *awsSession, profile string) (awsCredentialProvider, error) {
	cfg, err := s.configFiles()
	if err != nil {
		return nil, err
	}
	return awsStaticFromSection(cfg.credentials[profile], "shared-credentials-file")
}

func awsConfigFileProvider(s *awsSession, profile string) (awsCredentialProvider, error) {
	cfg, err := s.configFiles()
	if err != nil {
		return nil, err
	}
	return awsStaticFromSection(cfg.profiles[profile], "config-file")
}

// Reports a profile that carries a complete static key pair
func awsHasStaticKeys(p map[string]string) bool {
	return p["aws_access_key_id"] != "" && p["aws_secret_access_key"] != ""
}

// ---- credential_process ----

// Output contract of a credential_process command
type awsProcessOutput struct {
	Version         *int   `json:"Version"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken"`
	Expiration      string `json:"Expiration"`
}

// Runs the profile credential_process and parses its version 1 JSON
func awsProcessProvider(s *awsSession, profile string) (awsCredentialProvider, error) {
	p, err := s.profile(profile)
	if err != nil {
		return nil, err
	}
	command := p["credential_process"]
	if command == "" {
		return nil, nil
	}
	fetch := func(ctx context.Context) (awsCredentials, error) { return awsRunCredentialProcess(ctx, command) }
	creds, err := fetch(context.Background())
	if err != nil {
		return nil, err
	}
	if creds.Expires.IsZero() {
		return awsStaticProvider{creds}, nil
	}
	return newAWSRefreshing(fetch, &creds), nil
}

func awsRunCredentialProcess(ctx context.Context, command string) (awsCredentials, error) {
	argv, err := awsShellSplit(command)
	if err != nil {
		return awsCredentials{}, &AWSCredentialError{Provider: "custom-process", Message: err.Error()}
	}
	if len(argv) == 0 {
		return awsCredentials{}, &AWSCredentialError{Provider: "custom-process", Message: "credential_process is empty"}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return awsCredentials{}, &AWSCredentialError{Provider: "custom-process", Message: stderr.String()}
		}
		return awsCredentials{}, &AWSCredentialError{Provider: "custom-process", Message: err.Error(), Err: err}
	}
	var raw map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return awsCredentials{}, &AWSCredentialError{Provider: "custom-process", Message: err.Error(), Err: err}
	}
	var out awsProcessOutput
	_ = json.Unmarshal(stdout.Bytes(), &out)
	if out.Version == nil || *out.Version != 1 {
		version := "<Version key not provided>"
		if v, ok := raw["Version"]; ok {
			version = fmt.Sprint(v)
		}
		return awsCredentials{}, &AWSCredentialError{Provider: "custom-process", Message: fmt.Sprintf("Unsupported version '%s' for credential process provider, supported versions: 1", version)}
	}
	for _, key := range []string{"AccessKeyId", "SecretAccessKey"} {
		if _, ok := raw[key]; !ok {
			return awsCredentials{}, &AWSCredentialError{Provider: "custom-process", Message: fmt.Sprintf("Missing required key in response: '%s'", key)}
		}
	}
	creds := awsCredentials{AccessKeyID: out.AccessKeyID, SecretAccessKey: out.SecretAccessKey, SessionToken: out.SessionToken, Source: "custom-process"}
	if out.Expiration != "" {
		t, err := awsParseTime(out.Expiration)
		if err != nil {
			return awsCredentials{}, &AWSCredentialError{Provider: "custom-process", Message: err.Error()}
		}
		creds.Expires = t
	}
	return creds, nil
}

// ---- STS ----

type stsCredentialsXML struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey"`
	SessionToken    string `xml:"SessionToken"`
	Expiration      string `xml:"Expiration"`
}

type stsResponseXML struct {
	AssumeRole  struct{ Credentials stsCredentialsXML } `xml:"AssumeRoleResult"`
	WebIdentity struct{ Credentials stsCredentialsXML } `xml:"AssumeRoleWithWebIdentityResult"`
}

type awsXMLErrorResponse struct {
	Error struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Error"`
}

// AWSServiceError is an AWS API error response with its code and HTTP status
type AWSServiceError struct {
	Service    string
	Operation  string
	StatusCode int
	Code       string
	Message    string
	RequestID  string
	Body       string
	Headers    http.Header
}

func (e *AWSServiceError) Error() string {
	return fmt.Sprintf("An error occurred (%s) when calling the %s operation: %s", e.Code, e.Operation, e.Message)
}

// ResponseHeaders exposes the error response headers to the classifier
func (e *AWSServiceError) ResponseHeaders() http.Header { return e.Headers }

// Legacy STS regions answered by the global endpoint under sts_regional_endpoints=legacy
var awsSTSLegacyGlobalRegions = map[string]bool{
	"ap-northeast-1": true, "ap-south-1": true, "ap-southeast-1": true, "ap-southeast-2": true, "aws-global": true,
	"ca-central-1": true, "eu-central-1": true, "eu-north-1": true, "eu-west-1": true, "eu-west-2": true, "eu-west-3": true,
	"sa-east-1": true, "us-east-1": true, "us-east-2": true, "us-west-1": true, "us-west-2": true,
}

// STS endpoint and signing region for the session, configured endpoints winning
func (s *awsSession) stsEndpoint() (string, string) {
	region := s.region()
	signing := region
	if signing == "" || signing == "aws-global" {
		signing = "us-east-1"
	}
	if ep := s.configuredEndpoint("sts"); ep != "" {
		return ep, signing
	}
	mode := strings.ToLower(strings.TrimSpace(s.setting("AWS_STS_REGIONAL_ENDPOINTS", "sts_regional_endpoints")))
	if region == "" || region == "aws-global" || (mode == "legacy" && awsSTSLegacyGlobalRegions[region]) {
		return "https://sts.amazonaws.com", signing
	}
	return "https://sts." + region + "." + awsPartitionDNSSuffix(region), signing
}

// Posts one STS query action, signed with source credentials unless unsigned is requested
func (s *awsSession) callSTS(ctx context.Context, action string, params url.Values, signer *awsCredentials) (stsCredentialsXML, error) {
	endpoint, region := s.stsEndpoint()
	params.Set("Action", action)
	params.Set("Version", "2011-06-15")
	body := []byte(params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/", bytes.NewReader(body))
	if err != nil {
		return stsCredentialsXML{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("User-Agent", UserAgent("IcebathAgent"))
	if signer != nil {
		if _, err := sigV4Sign(req, body, *signer, "sts", region, awsNow()); err != nil {
			return stsCredentialsXML{}, err
		}
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return stsCredentialsXML{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return stsCredentialsXML{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var e awsXMLErrorResponse
		_ = xml.Unmarshal(data, &e)
		return stsCredentialsXML{}, &AWSServiceError{Service: "sts", Operation: action, StatusCode: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message, RequestID: resp.Header.Get("x-amzn-RequestId")}
	}
	var out stsResponseXML
	if err := xml.Unmarshal(data, &out); err != nil {
		return stsCredentialsXML{}, err
	}
	if action == "AssumeRoleWithWebIdentity" {
		return out.WebIdentity.Credentials, nil
	}
	return out.AssumeRole.Credentials, nil
}

func stsToCredentials(c stsCredentialsXML, source string) (awsCredentials, error) {
	creds := awsCredentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Source: source}
	if c.Expiration != "" {
		t, err := awsParseTime(c.Expiration)
		if err != nil {
			return awsCredentials{}, err
		}
		creds.Expires = t
	}
	if !creds.valid() {
		return awsCredentials{}, &AWSCredentialError{Provider: source, Message: "STS response carried no credentials"}
	}
	return creds, nil
}

// Default role session name botocore stamps on assumed roles
func awsDefaultRoleSessionName() string {
	return fmt.Sprintf("botocore-session-%d", awsNow().Unix())
}

// ---- assume role ----

// Prompt reading an MFA token code for a serial, replaced by tests
var awsMFAPrompt = func(serial string) (string, error) {
	return awsReadHiddenLine(fmt.Sprintf("Enter MFA code for %s: ", serial))
}

// Resolver for role_arn profiles, following source_profile chains with loop detection
type awsAssumeRoleResolver struct {
	s       *awsSession
	visited []string
}

func newAWSAssumeRoleResolver(s *awsSession) *awsAssumeRoleResolver {
	return &awsAssumeRoleResolver{s: s}
}

// Reports a profile configured for AssumeRole and not for web identity
func awsHasAssumeRoleVars(p map[string]string) bool {
	_, hasRole := p["role_arn"]
	_, hasWeb := p["web_identity_token_file"]
	return hasRole && !hasWeb
}

func (r *awsAssumeRoleResolver) load(ctx context.Context, profile string) (awsCredentialProvider, error) {
	cfg, err := r.s.configFiles()
	if err != nil {
		return nil, err
	}
	p := cfg.merged[profile]
	if !awsHasAssumeRoleVars(p) {
		return nil, nil
	}
	r.visited = []string{profile}
	return r.viaAssumeRole(ctx, profile)
}

func (r *awsAssumeRoleResolver) viaAssumeRole(ctx context.Context, profile string) (awsCredentialProvider, error) {
	cfg, _ := r.s.configFiles()
	p := cfg.merged[profile]
	source, credSource := p["source_profile"], p["credential_source"]
	_, hasSource := p["source_profile"]
	_, hasCredSource := p["credential_source"]
	switch {
	case hasSource && hasCredSource:
		return nil, &AWSCredentialError{Message: fmt.Sprintf("The profile %q contains both source_profile and credential_source.", profile)}
	case !hasSource && !hasCredSource:
		return nil, &AWSCredentialError{Provider: "assume-role", Message: "Partial credentials found in assume-role, missing: source_profile or credential_source"}
	case hasCredSource:
		if !awsValidCredentialSource(credSource) {
			return nil, &AWSCredentialError{Message: fmt.Sprintf("The credential_source %q is not a valid option.", credSource)}
		}
	default:
		if err := r.validateSourceProfile(profile, source); err != nil {
			return nil, err
		}
	}
	var sourceProvider awsCredentialProvider
	var err error
	if hasCredSource {
		sourceProvider, err = r.fromCredentialSource(ctx, credSource, profile)
	} else {
		r.visited = append(r.visited, source)
		sourceProvider, err = r.fromProfile(ctx, source)
	}
	if err != nil {
		return nil, err
	}
	params := url.Values{"RoleArn": {p["role_arn"]}}
	sessionName := p["role_session_name"]
	if v, ok := p["external_id"]; ok {
		params.Set("ExternalId", v)
	}
	mfaSerial, hasMFA := p["mfa_serial"]
	if hasMFA {
		params.Set("SerialNumber", mfaSerial)
	}
	if v, ok := p["duration_seconds"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			params.Set("DurationSeconds", strconv.Itoa(n))
		}
	}
	calls := 0
	fetch := func(ctx context.Context) (awsCredentials, error) {
		if hasMFA && calls > 0 {
			return awsCredentials{}, &AWSCredentialError{Message: "Cannot refresh credentials: MFA token required."}
		}
		calls++
		src, err := sourceProvider.Retrieve(ctx)
		if err != nil {
			return awsCredentials{}, err
		}
		q := url.Values{}
		for k, v := range params {
			q[k] = append([]string(nil), v...)
		}
		name := sessionName
		if name == "" {
			name = awsDefaultRoleSessionName()
		}
		q.Set("RoleSessionName", name)
		if hasMFA {
			code, err := awsMFAPrompt(mfaSerial)
			if err != nil {
				return awsCredentials{}, err
			}
			q.Set("TokenCode", code)
		}
		out, err := r.s.callSTS(ctx, "AssumeRole", q, &src)
		if err != nil {
			return awsCredentials{}, err
		}
		return stsToCredentials(out, "assume-role")
	}
	return newAWSRefreshing(fetch, nil), nil
}

func awsValidCredentialSource(name string) bool {
	switch strings.ToLower(name) {
	case "environment", "ec2instancemetadata", "ecscontainer":
		return true
	}
	return false
}

func (r *awsAssumeRoleResolver) validateSourceProfile(parent, source string) error {
	cfg, _ := r.s.configFiles()
	sp, ok := cfg.merged[source]
	if !ok {
		return &AWSCredentialError{Message: fmt.Sprintf("The source_profile %q referenced in the profile %q does not exist.", source, parent)}
	}
	seen := false
	for _, v := range r.visited {
		if v == source {
			seen = true
		}
	}
	if !seen {
		return nil
	}
	if source != parent || !awsHasStaticKeys(sp) {
		return &AWSCredentialError{Message: fmt.Sprintf("Infinite loop in credential configuration detected. Attempting to load from profile %s which has already been visited. Visited profiles: %s", source, strings.Join(r.visited, ", "))}
	}
	return nil
}

// Source credentials of a profile, static keys first, else its own chain, else a nested assume role
func (r *awsAssumeRoleResolver) fromProfile(ctx context.Context, profile string) (awsCredentialProvider, error) {
	cfg, _ := r.s.configFiles()
	p := cfg.merged[profile]
	if awsHasStaticKeys(p) || !awsHasAssumeRoleVars(p) {
		for _, link := range awsProfileLinks(profile, true) {
			prov, err := link.load(ctx, r.s)
			if err != nil {
				return nil, err
			}
			if prov != nil {
				return prov, nil
			}
		}
		return nil, &AWSCredentialError{Message: fmt.Sprintf("The source profile %q must have credentials.", profile)}
	}
	return r.viaAssumeRole(ctx, profile)
}

// Credentials of a canonical credential_source name
func (r *awsAssumeRoleResolver) fromCredentialSource(ctx context.Context, name, profile string) (awsCredentialProvider, error) {
	var prov awsCredentialProvider
	var err error
	switch strings.ToLower(name) {
	case "environment":
		prov, err = awsEnvProvider(r.s)
	case "ec2instancemetadata":
		prov, err = awsIMDSProvider(ctx, r.s)
	case "ecscontainer":
		prov, err = awsContainerProvider(ctx, r.s)
	}
	if err != nil {
		return nil, err
	}
	if prov == nil {
		return nil, &AWSCredentialError{Provider: name, Message: "No credentials found in credential_source referenced in profile " + profile}
	}
	return prov, nil
}

// ---- web identity ----

// AssumeRoleWithWebIdentity from the environment first then the profile token file and role
func awsWebIdentityProvider(s *awsSession, profile string, disableEnv bool) (awsCredentialProvider, error) {
	p, err := s.profile(profile)
	if err != nil {
		return nil, err
	}
	get := func(key, envName string) string {
		if !disableEnv {
			if v, ok := s.lookup(envName); ok {
				return v
			}
		}
		return p[key]
	}
	tokenFile := get("web_identity_token_file", "AWS_WEB_IDENTITY_TOKEN_FILE")
	if tokenFile == "" {
		return nil, nil
	}
	roleARN := get("role_arn", "AWS_ROLE_ARN")
	if roleARN == "" {
		return nil, &AWSCredentialError{Message: "The provided profile or the current environment is configured to assume role with web identity but has no role ARN configured. Ensure that the profile has the role_arn configuration set or the AWS_ROLE_ARN env var is set."}
	}
	sessionName := get("role_session_name", "AWS_ROLE_SESSION_NAME")
	fetch := func(ctx context.Context) (awsCredentials, error) {
		token, err := os.ReadFile(awsExpandPath(tokenFile))
		if err != nil {
			return awsCredentials{}, &AWSCredentialError{Provider: "assume-role-with-web-identity", Message: err.Error(), Err: err}
		}
		name := sessionName
		if name == "" {
			name = awsDefaultRoleSessionName()
		}
		q := url.Values{"RoleArn": {roleARN}, "RoleSessionName": {name}, "WebIdentityToken": {string(token)}}
		out, err := s.callSTS(ctx, "AssumeRoleWithWebIdentity", q, nil)
		if err != nil {
			return awsCredentials{}, err
		}
		return stsToCredentials(out, "assume-role-with-web-identity")
	}
	return newAWSRefreshing(fetch, nil), nil
}

// ---- SSO ----

var awsSSORequired = []string{"sso_start_url", "sso_region", "sso_role_name", "sso_account_id"}

// SSO role credentials for profiles carrying sso_role_name or sso_account_id, legacy or sso-session
func awsSSOProvider(s *awsSession, profile string) (awsCredentialProvider, error) {
	cfg, err := s.configFiles()
	if err != nil {
		return nil, err
	}
	p := cfg.merged[profile]
	_, hasRole := p["sso_role_name"]
	_, hasAccount := p["sso_account_id"]
	if !hasRole && !hasAccount {
		return nil, nil
	}
	resolved := copyStringMap(p)
	required := append([]string(nil), awsSSORequired...)
	sessionName, hasSession := p["sso_session"]
	if hasSession {
		session, ok := cfg.ssoSessions[sessionName]
		if !ok {
			return nil, &AWSCredentialError{Message: fmt.Sprintf("The specified sso-session does not exist: %q", sessionName)}
		}
		for k, v := range session {
			if prior, ok := resolved[k]; ok && prior != v {
				return nil, &AWSCredentialError{Message: fmt.Sprintf("The value for %s is inconsistent between profile (%s) and sso-session (%s)", k, prior, v)}
			}
			resolved[k] = v
		}
		resolved["session_name"] = sessionName
		required = append(required, "session_name")
	}
	var missing []string
	for _, k := range required {
		if _, ok := resolved[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, &AWSCredentialError{Message: fmt.Sprintf("The profile %q is configured to use SSO but is missing required configuration: %s", profile, strings.Join(missing, ", "))}
	}
	tokens := &awsSSOTokenSource{s: s, startURL: resolved["sso_start_url"], region: resolved["sso_region"], session: resolved["session_name"]}
	fetch := func(ctx context.Context) (awsCredentials, error) {
		token, err := tokens.token(ctx)
		if err != nil {
			return awsCredentials{}, err
		}
		return s.ssoRoleCredentials(ctx, resolved["sso_region"], resolved["sso_account_id"], resolved["sso_role_name"], token)
	}
	return newAWSRefreshing(fetch, nil), nil
}

// Cached SSO token file of the aws sso login flow
type awsSSOToken struct {
	AccessToken           string `json:"accessToken"`
	ExpiresAt             string `json:"expiresAt"`
	RefreshToken          string `json:"refreshToken,omitempty"`
	ClientID              string `json:"clientId,omitempty"`
	ClientSecret          string `json:"clientSecret,omitempty"`
	RegistrationExpiresAt string `json:"registrationExpiresAt,omitempty"`
	Region                string `json:"region,omitempty"`
	StartURL              string `json:"startUrl,omitempty"`
}

// Token file path keyed by the sha1 of the session name, or of the start URL for legacy profiles
func awsSSOCachePath(startURL, session string) string {
	key := startURL
	if session != "" {
		key = session
	}
	sum := sha1.Sum([]byte(key))
	h, err := os.UserHomeDir()
	if err != nil {
		h = ""
	}
	return filepath.Join(h, ".aws", "sso", "cache", hex.EncodeToString(sum[:])+".json")
}

// SSO access token loader refreshing sso-session tokens through sso-oidc inside the 15 minute window
type awsSSOTokenSource struct {
	s        *awsSession
	startURL string
	region   string
	session  string
}

func (t *awsSSOTokenSource) load() (map[string]any, awsSSOToken, error) {
	path := awsSSOCachePath(t.startURL, t.session)
	data, err := os.ReadFile(path)
	if err != nil {
		subject := t.startURL
		if t.session != "" {
			subject = t.session
		}
		return nil, awsSSOToken{}, &AWSCredentialError{Message: fmt.Sprintf("Error loading SSO Token: Token for %s does not exist", subject), Err: err}
	}
	var raw map[string]any
	var tok awsSSOToken
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, awsSSOToken{}, &AWSCredentialError{Message: "Error loading SSO Token: " + err.Error(), Err: err}
	}
	_ = json.Unmarshal(data, &tok)
	if tok.AccessToken == "" || tok.ExpiresAt == "" {
		return nil, awsSSOToken{}, &AWSCredentialError{Message: fmt.Sprintf("Error loading SSO Token: Token for %s is invalid", t.startURL)}
	}
	return raw, tok, nil
}

func (t *awsSSOTokenSource) token(ctx context.Context) (string, error) {
	raw, tok, err := t.load()
	if err != nil {
		return "", err
	}
	if t.session == "" {
		return tok.AccessToken, nil
	}
	expires, err := awsParseTime(tok.ExpiresAt)
	if err != nil {
		return "", &AWSCredentialError{Message: "Error loading SSO Token: " + err.Error()}
	}
	now := awsNow()
	if expires.Sub(now) < awsAdvisoryRefresh {
		if fresh, ok := t.refresh(ctx, tok); ok {
			for k, v := range map[string]any{"startUrl": t.startURL, "region": t.region, "accessToken": fresh.AccessToken,
				"expiresAt": fresh.ExpiresAt, "clientId": tok.ClientID, "clientSecret": tok.ClientSecret, "registrationExpiresAt": tok.RegistrationExpiresAt} {
				raw[k] = v
			}
			if fresh.RefreshToken != "" {
				raw["refreshToken"] = fresh.RefreshToken
			}
			if data, err := json.Marshal(raw); err == nil {
				path := awsSSOCachePath(t.startURL, t.session)
				_ = os.MkdirAll(filepath.Dir(path), 0o700)
				_ = os.WriteFile(path, data, 0o600)
			}
			tok, expires = fresh, mustAWSTime(fresh.ExpiresAt)
		}
	}
	if !expires.After(awsNow()) {
		return "", &AWSCredentialError{Provider: "sso", Message: "Token has expired and refresh failed"}
	}
	return tok.AccessToken, nil
}

func mustAWSTime(v string) time.Time {
	t, _ := awsParseTime(v)
	return t
}

// Exchanges the cached refresh token through sso-oidc CreateToken, false when it cannot refresh
func (t *awsSSOTokenSource) refresh(ctx context.Context, tok awsSSOToken) (awsSSOToken, bool) {
	if tok.RefreshToken == "" || tok.ClientID == "" || tok.ClientSecret == "" || tok.RegistrationExpiresAt == "" {
		return awsSSOToken{}, false
	}
	reg, err := awsParseTime(tok.RegistrationExpiresAt)
	if err != nil || !reg.After(awsNow()) {
		return awsSSOToken{}, false
	}
	endpoint := t.s.configuredEndpoint("sso-oidc")
	if endpoint == "" {
		endpoint = "https://oidc." + t.region + "." + awsPartitionDNSSuffix(t.region)
	}
	body, _ := json.Marshal(map[string]string{"grantType": "refresh_token", "clientId": tok.ClientID, "clientSecret": tok.ClientSecret, "refreshToken": tok.RefreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/token", bytes.NewReader(body))
	if err != nil {
		return awsSSOToken{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.s.http.Do(req)
	if err != nil {
		return awsSSOToken{}, false
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return awsSSOToken{}, false
	}
	var out struct {
		AccessToken  string  `json:"accessToken"`
		ExpiresIn    float64 `json:"expiresIn"`
		RefreshToken string  `json:"refreshToken"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.AccessToken == "" {
		return awsSSOToken{}, false
	}
	return awsSSOToken{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken,
		ExpiresAt: awsNow().Add(time.Duration(out.ExpiresIn * float64(time.Second))).UTC().Format(time.RFC3339)}, true
}

// SSO portal GetRoleCredentials for one account and role
func (s *awsSession) ssoRoleCredentials(ctx context.Context, region, account, role, token string) (awsCredentials, error) {
	endpoint := s.configuredEndpoint("sso")
	if endpoint == "" {
		endpoint = "https://portal.sso." + region + "." + awsPartitionDNSSuffix(region)
	}
	q := url.Values{"account_id": {account}, "role_name": {role}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/federation/credentials?"+q.Encode(), nil)
	if err != nil {
		return awsCredentials{}, err
	}
	req.Header.Set("x-amz-sso_bearer_token", token)
	resp, err := s.http.Do(req)
	if err != nil {
		return awsCredentials{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return awsCredentials{}, &AWSCredentialError{Provider: "sso", Message: "The SSO session associated with this profile has expired or is otherwise invalid. To refresh this SSO session run aws sso login with the corresponding profile."}
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return awsCredentials{}, &AWSServiceError{Service: "sso", Operation: "GetRoleCredentials", StatusCode: resp.StatusCode, Code: awsErrorTypeHeader(resp.Header), Message: e.Message}
	}
	var out struct {
		RoleCredentials struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
			Expiration      int64  `json:"expiration"`
		} `json:"roleCredentials"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return awsCredentials{}, err
	}
	rc := out.RoleCredentials
	return awsCredentials{AccessKeyID: rc.AccessKeyID, SecretAccessKey: rc.SecretAccessKey, SessionToken: rc.SessionToken,
		Expires: time.UnixMilli(rc.Expiration).UTC(), Source: "sso"}, nil
}

// Error code of an AWS JSON error from the x-amzn-ErrorType header, the part before the colon
func awsErrorTypeHeader(h http.Header) string {
	v := h.Get("x-amzn-ErrorType")
	if i := strings.IndexByte(v, ':'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// ---- container ----

var awsContainerAllowedHosts = map[string]bool{"169.254.170.2": true, "169.254.170.23": true, "fd00:ec2::23": true, "localhost": true}

// Delay between container metadata attempts, replaced by tests
var awsContainerRetrySleep = time.Second

// ECS and EKS container credentials from the relative or full URI with the authorization token
func awsContainerProvider(ctx context.Context, s *awsSession) (awsCredentialProvider, error) {
	if !s.hasEnv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") && !s.hasEnv("AWS_CONTAINER_CREDENTIALS_FULL_URI") {
		return nil, nil
	}
	full := ""
	if rel, ok := s.lookup("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"); ok {
		full = "http://169.254.170.2" + rel
	} else {
		full = s.env("AWS_CONTAINER_CREDENTIALS_FULL_URI")
		u, err := url.Parse(full)
		if err != nil {
			return nil, &AWSCredentialError{Provider: "container-role", Message: err.Error(), Err: err}
		}
		host := u.Hostname()
		ip := net.ParseIP(host)
		if !(ip != nil && ip.IsLoopback()) && !awsContainerAllowedHosts[host] {
			return nil, &AWSCredentialError{Provider: "container-role", Message: fmt.Sprintf("Unsupported host '%s'.  Can only retrieve metadata from a loopback address or one of these hosts: 169.254.170.2, 169.254.170.23, fd00:ec2::23, localhost", host)}
		}
	}
	fetch := func(ctx context.Context) (awsCredentials, error) {
		headers := map[string]string{"Accept": "application/json"}
		var token string
		if path, ok := s.lookup("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); ok {
			data, err := os.ReadFile(path)
			if err != nil {
				return awsCredentials{}, &AWSCredentialError{Provider: "container-role", Message: err.Error(), Err: err}
			}
			token = string(data)
		} else if v, ok := s.lookup("AWS_CONTAINER_AUTHORIZATION_TOKEN"); ok {
			token = v
		}
		if token != "" || s.hasEnv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE") || s.hasEnv("AWS_CONTAINER_AUTHORIZATION_TOKEN") {
			if strings.ContainsAny(token, "\r\n") {
				return awsCredentials{}, &AWSCredentialError{Provider: "container-role", Message: "Auth token value is not a legal header value"}
			}
			headers["Authorization"] = token
		}
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return awsCredentials{}, ctx.Err()
				case <-time.After(awsContainerRetrySleep):
				}
			}
			creds, err := awsContainerFetchOnce(ctx, s.http, full, headers)
			if err == nil {
				return creds, nil
			}
			lastErr = err
		}
		return awsCredentials{}, &AWSCredentialError{Provider: "container-role", Message: lastErr.Error(), Err: lastErr}
	}
	creds, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	return newAWSRefreshing(fetch, &creds), nil
}

func awsContainerFetchOnce(ctx context.Context, client *http.Client, full string, headers map[string]string) (awsCredentials, error) {
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, full, nil)
	if err != nil {
		return awsCredentials{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return awsCredentials{}, fmt.Errorf("Error retrieving metadata: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return awsCredentials{}, fmt.Errorf("Received non 200 response %d from container metadata: %s", resp.StatusCode, string(data))
	}
	var out struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		Token           string `json:"Token"`
		Expiration      string `json:"Expiration"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return awsCredentials{}, errors.New("Unable to parse JSON returned from container metadata services")
	}
	creds := awsCredentials{AccessKeyID: out.AccessKeyID, SecretAccessKey: out.SecretAccessKey, SessionToken: out.Token, Source: "container-role"}
	if out.Expiration != "" {
		t, err := awsParseTime(out.Expiration)
		if err != nil {
			return awsCredentials{}, err
		}
		creds.Expires = t
	}
	return creds, nil
}

// ---- IMDS ----

// IMDSv2 settings of the session, endpoint, timeout, attempts and the v1 fallback switch
type awsIMDSConfig struct {
	endpoint   string
	timeout    time.Duration
	attempts   int
	disabled   bool
	v1Disabled bool
}

func (s *awsSession) imdsConfig() awsIMDSConfig {
	c := awsIMDSConfig{endpoint: "http://169.254.169.254/", timeout: time.Second, attempts: 1}
	if v := s.setting("AWS_EC2_METADATA_SERVICE_ENDPOINT", "ec2_metadata_service_endpoint"); v != "" {
		c.endpoint = strings.TrimRight(v, "/") + "/"
	} else if strings.EqualFold(s.setting("AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE", "ec2_metadata_service_endpoint_mode"), "ipv6") {
		c.endpoint = "http://[fd00:ec2::254]/"
	}
	if v := s.setting("AWS_METADATA_SERVICE_TIMEOUT", "metadata_service_timeout"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			c.timeout = time.Duration(f * float64(time.Second))
		}
	}
	if v := s.setting("AWS_METADATA_SERVICE_NUM_ATTEMPTS", "metadata_service_num_attempts"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.attempts = n
		}
	}
	c.disabled = s.opts.DisableIMDS || awsEnsureBool(s.setting("AWS_EC2_METADATA_DISABLED", "ec2_metadata_disabled"))
	c.v1Disabled = awsEnsureBool(s.setting("AWS_EC2_METADATA_V1_DISABLED", "ec2_metadata_v1_disabled"))
	return c
}

// Instance role credentials over IMDSv2, nil when metadata is disabled or answers nothing usable
func awsIMDSProvider(ctx context.Context, s *awsSession) (awsCredentialProvider, error) {
	cfg := s.imdsConfig()
	if cfg.disabled {
		return nil, nil
	}
	fetch := func(ctx context.Context) (awsCredentials, error) {
		creds, ok, err := awsIMDSFetch(ctx, s.http, cfg)
		if err != nil {
			return awsCredentials{}, err
		}
		if !ok {
			return awsCredentials{}, &AWSCredentialError{Provider: "iam-role", Message: "instance metadata returned no credentials"}
		}
		return creds, nil
	}
	creds, ok, err := awsIMDSFetch(ctx, s.http, cfg)
	if err != nil || !ok {
		return nil, nil
	}
	return newAWSRefreshing(fetch, &creds), nil
}

// Token PUT with a six hour TTL, falling back to IMDSv1 on 403, 404 and 405 unless v1 is disabled
func awsIMDSToken(ctx context.Context, client *http.Client, cfg awsIMDSConfig) (string, bool, error) {
	for i := 0; i < cfg.attempts; i++ {
		rctx, cancel := context.WithTimeout(ctx, cfg.timeout)
		req, err := http.NewRequestWithContext(rctx, http.MethodPut, cfg.endpoint+"latest/api/token", nil)
		if err != nil {
			cancel()
			return "", false, err
		}
		req.Header.Set("x-aws-ec2-metadata-token-ttl-seconds", "21600")
		req.Header.Set("User-Agent", UserAgent("IcebathAgent"))
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return "", false, nil
			}
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		switch resp.StatusCode {
		case http.StatusOK:
			return string(data), true, nil
		case http.StatusNotFound, http.StatusForbidden, http.StatusMethodNotAllowed:
			return "", false, nil
		case http.StatusBadRequest:
			return "", false, errors.New("Bad IMDS request")
		}
	}
	return "", false, nil
}

// GET of one metadata path with the retry predicate botocore applies
func awsIMDSGet(ctx context.Context, client *http.Client, cfg awsIMDSConfig, path, token string, ok func(int, []byte) bool) ([]byte, bool) {
	for i := 0; i < cfg.attempts; i++ {
		rctx, cancel := context.WithTimeout(ctx, cfg.timeout)
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, cfg.endpoint+path, nil)
		if err != nil {
			cancel()
			return nil, false
		}
		req.Header.Set("User-Agent", UserAgent("IcebathAgent"))
		if token != "" {
			req.Header.Set("x-aws-ec2-metadata-token", token)
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if ok(resp.StatusCode, data) {
			return data, true
		}
	}
	return nil, false
}

// Role name then the role credentials, extending an expiry inside the refresh window for static stability
func awsIMDSFetch(ctx context.Context, client *http.Client, cfg awsIMDSConfig) (awsCredentials, bool, error) {
	token, haveToken, err := awsIMDSToken(ctx, client, cfg)
	if err != nil {
		return awsCredentials{}, false, nil
	}
	if !haveToken && cfg.v1Disabled {
		return awsCredentials{}, false, nil
	}
	const base = "latest/meta-data/iam/security-credentials/"
	roleData, ok := awsIMDSGet(ctx, client, cfg, base, token, func(status int, data []byte) bool { return status == 200 && len(data) > 0 })
	if !ok {
		return awsCredentials{}, false, nil
	}
	role := strings.TrimSpace(string(roleData))
	credData, ok := awsIMDSGet(ctx, client, cfg, base+role, token, func(status int, data []byte) bool {
		var probe map[string]any
		return status == 200 && len(data) > 0 && json.Unmarshal(data, &probe) == nil
	})
	if !ok {
		return awsCredentials{}, false, nil
	}
	var out map[string]any
	_ = json.Unmarshal(credData, &out)
	for _, k := range []string{"AccessKeyId", "SecretAccessKey", "Token", "Expiration"} {
		if _, has := out[k]; !has {
			return awsCredentials{}, false, nil
		}
	}
	creds := awsCredentials{AccessKeyID: asStr(out["AccessKeyId"]), SecretAccessKey: asStr(out["SecretAccessKey"]), SessionToken: asStr(out["Token"]), Source: "iam-role"}
	if exp, err := awsParseTime(asStr(out["Expiration"])); err == nil {
		offset := 10*time.Minute + time.Duration(120+rand.Intn(481))*time.Second
		now := awsNow().UTC()
		if !now.Before(exp.Add(-offset)) {
			exp = now.Add(offset)
		}
		creds.Expires = exp
	}
	return creds, true, nil
}

// ---- chain probes ----

// Walks a fresh default chain and reports whether it yields an access key
func awsChainHasCredentials(instanceMetadata bool) bool {
	ctx := context.Background()
	s := newAWSSession(awsSessionOptions{DisableIMDS: !instanceMetadata})
	src, err := resolveAWSCredentialSource(ctx, s)
	if err != nil {
		return false
	}
	creds, err := src.Retrieve(ctx)
	return err == nil && creds.AccessKeyID != ""
}
