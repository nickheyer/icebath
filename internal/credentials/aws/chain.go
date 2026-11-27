package aws

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
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

	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Windows ahead of expiry in which keys are renewed: when they may still be used, and past which they may not
const (
	advisory  = 15 * time.Minute
	mandatory = 10 * time.Minute
)

// The chain walked to its end with no source answering
var ErrNoCredentials = errors.New("No AWS sign-in was found. Run `aws sso login` or `aws configure`, or set AWS_PROFILE.")

// Failure of one source of the chain, naming it
type Error struct {
	Source  string
	Message string
	Err     error
}

func (e *Error) Error() string {
	message := strings.TrimSpace(e.Message)
	if message != "" && !strings.ContainsAny(message[len(message)-1:], ".!?") {
		message += "."
	}
	if where := sourceWords[e.Source]; where != "" && !strings.Contains(message, where) {
		return message + " The AWS credentials came " + where + "."
	}
	return message
}

// Where each source of the chain takes AWS credentials from, said after "came"
var sourceWords = map[string]string{"env": "from environment variables", "custom-process": "from credential_process", "sso": "from AWS SSO",
	"container-role": "from the container's role", "iam-role": "from the instance's role", "assume-role": "from an assumed role",
	"assume-role-with-web-identity": "from a web identity role", "shared-credentials-file": "from the AWS credentials file",
	"config-file": "from the AWS config file"}

func (e *Error) Unwrap() error { return e.Err }

// Error answer of an AWS API
type ServiceError struct {
	Operation  string
	StatusCode int
	Code       string
	Message    string
}

func (e *ServiceError) Error() string {
	return fmt.Sprintf("AWS refused %s with %s: %s", e.Operation, e.Code, strings.TrimSuffix(strings.TrimSpace(e.Message), ".")+".")
}

// Source of keys, renewing them as they near expiry
type Provider interface {
	Retrieve(ctx context.Context) (Keys, error)
}

type static struct{ keys Keys }

func (s static) Retrieve(context.Context) (Keys, error) { return s.keys, nil }

// Keys of one source kept until the advisory window, renewed inside it, and refused inside the mandatory one when the renewal fails
type refreshing struct {
	mu    sync.Mutex
	fetch func(ctx context.Context) (Keys, error)
	keys  Keys
	have  bool
}

func renewing(fetch func(ctx context.Context) (Keys, error), initial *Keys) *refreshing {
	r := &refreshing{fetch: fetch}
	if initial != nil {
		r.keys, r.have = *initial, true
	}
	return r
}

func (r *refreshing) Retrieve(ctx context.Context) (Keys, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if r.have && (r.keys.Expires.IsZero() || r.keys.Expires.Sub(now) > advisory) {
		return r.keys, nil
	}
	fresh, err := r.fetch(ctx)
	if err == nil && !fresh.Valid() {
		err = &Error{Source: fresh.Source, Message: "The renewed AWS credentials lack the access key ID or the secret access key."}
	}
	if err != nil {
		if r.have && r.keys.Expires.Sub(now) > mandatory {
			return r.keys, nil
		}
		return Keys{}, err
	}
	r.keys, r.have = fresh, true
	if !fresh.Expires.IsZero() && !fresh.Expires.After(time.Now()) {
		return Keys{}, &Error{Source: fresh.Source, Message: "The AWS credentials were renewed, but the renewed ones have already expired."}
	}
	return r.keys, nil
}

// What a chain is asked for, as an AWS SDK session takes it
type Options struct {
	// Profile of the shared config, AWS_PROFILE and then default when empty; naming one turns the environment's keys off
	Profile string
	// Leaves the instance metadata service out
	NoIMDS bool
	// Asks for an MFA code a role names, nil to read it from the terminal
	MFA func(ctx context.Context, prompt string) (string, error)
	// Reads a variable, os.LookupEnv when nil
	Lookup func(name string) (string, bool)
}

// The default chain of one session: walked once, then renewed through the source that answered
type Chain struct {
	s      *session
	mu     sync.Mutex
	source Provider
	name   string
}

func NewChain(opts Options) *Chain {
	if opts.Lookup == nil {
		opts.Lookup = os.LookupEnv
	}
	if opts.MFA == nil {
		opts.MFA = readMFA
	}
	return &Chain{s: &session{opts: opts, http: &http.Client{}}}
}

func (c *Chain) Retrieve(ctx context.Context) (Keys, error) {
	c.mu.Lock()
	source := c.source
	c.mu.Unlock()
	if source == nil {
		resolved, name, err := resolve(ctx, c.s)
		if err != nil {
			return Keys{}, err
		}
		c.mu.Lock()
		if c.source == nil {
			c.source, c.name = resolved, name
		}
		source = c.source
		c.mu.Unlock()
	}
	return source.Retrieve(ctx)
}

// Region the session's environment or profile names, empty when neither does
func (c *Chain) Region() string {
	return strings.TrimSpace(c.s.setting("AWS_DEFAULT_REGION", "region"))
}

// Profile in use
func (c *Chain) Profile() string {
	return c.s.profileName()
}

// Session over the environment and the shared config files
type session struct {
	opts   Options
	http   *http.Client
	once   sync.Once
	files  *files
	failed error
}

func (s *session) env(name string) string {
	v, _ := s.opts.Lookup(name)
	return v
}

func (s *session) has(name string) bool {
	_, ok := s.opts.Lookup(name)
	return ok
}

func (s *session) profileName() string {
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

func (s *session) shared() (*files, error) {
	s.once.Do(func() { s.files, s.failed = load(s.env("AWS_CONFIG_FILE"), s.env("AWS_SHARED_CREDENTIALS_FILE")) })
	return s.files, s.failed
}

// Values of a profile, the config and credentials files merged; an unknown named profile is an error and an absent default is empty
func (s *session) profile(name string) (map[string]string, error) {
	f, err := s.shared()
	if err != nil {
		return nil, err
	}
	if p, ok := f.merged[name]; ok {
		return p, nil
	}
	if name == "default" {
		return map[string]string{}, nil
	}
	return nil, &Error{Message: fmt.Sprintf("Your AWS config has no profile named %s.", name)}
}

// A setting from its variable, else from the profile in use
func (s *session) setting(variable, key string) string {
	if variable != "" {
		if v, ok := s.opts.Lookup(variable); ok {
			return v
		}
	}
	p, err := s.profile(s.profileName())
	if err != nil {
		return ""
	}
	return p[key]
}

func truthy(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "true") }

// Endpoint configured for a service: its variable, the global one, the profile's services section, then its endpoint_url
func (s *session) endpoint(service string) string {
	snake := strings.ReplaceAll(strings.ToLower(service), "-", "_")
	if v, ok := s.opts.Lookup("AWS_IGNORE_CONFIGURED_ENDPOINT_URLS"); ok {
		if truthy(v) {
			return ""
		}
	} else if p, err := s.profile(s.profileName()); err == nil && truthy(p["ignore_configured_endpoint_urls"]) {
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
		if f, err := s.shared(); err == nil {
			if svc, ok := f.services[section][snake]; ok && svc["endpoint_url"] != "" {
				return svc["endpoint_url"]
			}
		}
	}
	return p["endpoint_url"]
}

// DNS suffix of the partition a region is in
func Suffix(region string) string {
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

// The shared config and credentials files
type files struct {
	profiles, credentials, merged, sso map[string]map[string]string
	services                           map[string]map[string]map[string]string
}

// Path set in a variable, with variables and a leading ~ expanded
func expand(path string) string {
	path = os.ExpandEnv(path)
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

func file(override, name string) string {
	if override != "" {
		return expand(override)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".aws", name)
}

// Reads both shared files, a missing one as empty
func load(configOverride, credentialsOverride string) (*files, error) {
	out := &files{profiles: map[string]map[string]string{}, credentials: map[string]map[string]string{}, merged: map[string]map[string]string{},
		sso: map[string]map[string]string{}, services: map[string]map[string]map[string]string{}}
	config, err := readINI(file(configOverride, "config"))
	if err != nil {
		return nil, err
	}
	for _, name := range config.order {
		values := config.flat[name]
		words, _ := terminal.Split(name)
		switch {
		case strings.HasPrefix(name, "profile") && len(words) == 2:
			out.profiles[words[1]] = values
		case strings.HasPrefix(name, "sso-session") && len(words) == 2:
			out.sso[words[1]] = values
		case strings.HasPrefix(name, "services") && len(words) == 2:
			out.services[words[1]] = config.nested[name]
		case name == "default":
			out.profiles["default"] = values
		}
	}
	creds, err := readINI(file(credentialsOverride, "credentials"))
	if err != nil {
		return nil, err
	}
	for _, name := range creds.order {
		out.credentials[name] = creds.flat[name]
	}
	for name, p := range out.profiles {
		out.merged[name] = clone(p)
	}
	for name, p := range out.credentials {
		merged, ok := out.merged[name]
		if !ok {
			merged = map[string]string{}
			out.merged[name] = merged
		}
		for k, v := range p {
			merged[k] = v
		}
	}
	return out, nil
}

func clone(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// One INI document: its sections in order, their values, and the one level of nesting the AWS config has
type ini struct {
	order  []string
	flat   map[string]map[string]string
	nested map[string]map[string]map[string]string
}

func readINI(path string) (*ini, error) {
	empty := &ini{flat: map[string]map[string]string{}, nested: map[string]map[string]map[string]string{}}
	if path == "" {
		return empty, nil
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return empty, nil
	}
	data, err := os.ReadFile(path)
	if err == nil {
		var doc *ini
		if doc, err = parseINI(string(data)); err == nil {
			return doc, nil
		}
	}
	return nil, &Error{Message: fmt.Sprintf("The AWS config file %s can't be read.", path), Err: err}
}

var option = regexp.MustCompile(`^(.*?)\s*([=:])\s*(.*)$`)

// configparser's rules: duplicates refused, DEFAULT inherited, indented lines continuing an option
func parseINI(text string) (*ini, error) {
	type section struct{ values map[string][]string }
	sections := map[string]*section{}
	var order []string
	defaults := &section{values: map[string][]string{}}
	var current *section
	key, indent := "", 0
	var bad []string
	for n, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		stripped := strings.TrimSpace(line)
		if strings.HasPrefix(stripped, "#") || strings.HasPrefix(stripped, ";") {
			continue
		}
		if stripped == "" {
			if current != nil && key != "" {
				current.values[key] = append(current.values[key], "")
			}
			continue
		}
		lead := len(line) - len(strings.TrimLeft(line, " \t"))
		if current != nil && key != "" && lead > indent {
			current.values[key] = append(current.values[key], stripped)
			continue
		}
		indent = lead
		if strings.HasPrefix(stripped, "[") {
			if end := strings.LastIndex(stripped, "]"); end > 1 {
				name := stripped[1:end]
				key = ""
				if name == "DEFAULT" {
					current = defaults
					continue
				}
				if _, dup := sections[name]; dup {
					return nil, fmt.Errorf("Your AWS config file has the section %q twice. Remove one of them.", name)
				}
				current = &section{values: map[string][]string{}}
				sections[name] = current
				order = append(order, name)
				continue
			}
		}
		if current == nil {
			return nil, fmt.Errorf("Line %d of your AWS config file comes before any section: %q. Put it under a section such as [default].", n+1, line)
		}
		m := option.FindStringSubmatch(stripped)
		if m == nil || strings.TrimSpace(m[1]) == "" {
			bad = append(bad, fmt.Sprintf("[line %2d]: %q", n+1, line))
			key = ""
			continue
		}
		name := strings.ToLower(strings.TrimRight(m[1], " \t"))
		if _, dup := current.values[name]; dup {
			return nil, fmt.Errorf("A section of your AWS config file sets %q twice. Remove one of them.", name)
		}
		current.values[name] = []string{strings.TrimSpace(m[3])}
		key = name
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("Your AWS config file has lines that can't be read: %s.", strings.Join(bad, " "))
	}
	doc := &ini{order: order, flat: map[string]map[string]string{}, nested: map[string]map[string]map[string]string{}}
	for _, name := range order {
		merged := map[string][]string{}
		for k, v := range defaults.values {
			merged[k] = v
		}
		for k, v := range sections[name].values {
			merged[k] = v
		}
		flat, nested := map[string]string{}, map[string]map[string]string{}
		for k, v := range merged {
			value := strings.TrimRight(strings.Join(v, "\n"), " \t\n")
			if len(v) > 1 && v[0] == "" {
				sub := map[string]string{}
				for _, line := range strings.Split(value, "\n") {
					if line = strings.TrimSpace(line); line == "" {
						continue
					}
					k, v, ok := strings.Cut(line, "=")
					if !ok {
						return nil, fmt.Errorf("The nested line %q of your AWS config file has no value. Write it as name = value.", line)
					}
					sub[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
				nested[k] = sub
			}
			flat[k] = value
		}
		doc.flat[name], doc.nested[name] = flat, nested
	}
	return doc, nil
}

// Words of a command line as the platform splits them: the C runtime's rules on Windows, a POSIX shell's elsewhere
func split(command string) ([]string, error) {
	if runtime.GOOS != "windows" {
		return terminal.Split(command)
	}
	var words []string
	var word strings.Builder
	quoted, slashes, started := false, 0, false
	flush := func() {
		word.WriteString(strings.Repeat(`\`, slashes))
		slashes = 0
	}
	for _, ch := range command {
		switch {
		case ch == '\\':
			slashes++
			started = true
		case ch == '"':
			word.WriteString(strings.Repeat(`\`, slashes/2))
			odd := slashes%2 == 1
			slashes, started = 0, true
			if odd {
				word.WriteRune('"')
				continue
			}
			quoted = !quoted
		case (ch == ' ' || ch == '\t') && !quoted:
			flush()
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		default:
			flush()
			word.WriteRune(ch)
			started = true
		}
	}
	if quoted {
		return nil, fmt.Errorf("A quote is not closed in the AWS credential command: %s", command)
	}
	flush()
	if started {
		words = append(words, word.String())
	}
	return words, nil
}

// Timestamp as AWS services and tools write them
func parseTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if strings.HasSuffix(v, "UTC") {
		v = strings.TrimSuffix(v, "UTC") + "Z"
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05", "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("The time %q in AWS credentials can't be read.", v)
}

// One source of the chain, a nil provider when it doesn't apply
type link struct {
	name string
	load func(ctx context.Context, s *session) (Provider, error)
}

// Walks the chain: the environment's keys, an assumed role, web identity, SSO, the credentials file, credential_process, the config
// file, the container's role and the instance's role. The source that answered and its name
func resolve(ctx context.Context, s *session) (Provider, string, error) {
	useEnv := s.opts.Profile == ""
	profile := s.profileName()
	if _, err := s.profile(profile); err != nil {
		return nil, "", err
	}
	var links []link
	if useEnv {
		links = append(links, link{"env", func(ctx context.Context, s *session) (Provider, error) { return fromEnv(s) }})
	}
	links = append(links, link{"assume-role", func(ctx context.Context, s *session) (Provider, error) { return (&roles{s: s}).load(ctx, profile) }})
	links = append(links, profiled(profile, !useEnv)...)
	links = append(links,
		link{"container-role", func(ctx context.Context, s *session) (Provider, error) { return container(ctx, s) }},
		link{"iam-role", func(ctx context.Context, s *session) (Provider, error) { return instance(ctx, s) }})
	for _, l := range links {
		p, err := l.load(ctx, s)
		if err != nil {
			return nil, "", err
		}
		if p != nil {
			return p, l.name, nil
		}
	}
	return nil, "", ErrNoCredentials
}

// Sources of one profile: web identity, SSO, the credentials file, credential_process and the config file
func profiled(profile string, noEnv bool) []link {
	return []link{
		{"assume-role-with-web-identity", func(ctx context.Context, s *session) (Provider, error) { return webIdentity(s, profile, noEnv) }},
		{"sso", func(ctx context.Context, s *session) (Provider, error) { return sso(s, profile) }},
		{"shared-credentials-file", func(ctx context.Context, s *session) (Provider, error) {
			f, err := s.shared()
			if err != nil {
				return nil, err
			}
			return section(f.credentials[profile], "shared-credentials-file")
		}},
		{"custom-process", func(ctx context.Context, s *session) (Provider, error) { return process(ctx, s, profile) }},
		{"config-file", func(ctx context.Context, s *session) (Provider, error) {
			f, err := s.shared()
			if err != nil {
				return nil, err
			}
			return section(f.profiles[profile], "config-file")
		}},
	}
}

// Keys of the AWS_ variables, renewed from them when they say when they expire; half a pair is an error
func fromEnv(s *session) (Provider, error) {
	if s.env("AWS_ACCESS_KEY_ID") == "" {
		return nil, nil
	}
	read := func(context.Context) (Keys, error) {
		keys := Keys{AccessKeyID: s.env("AWS_ACCESS_KEY_ID"), SecretAccessKey: s.env("AWS_SECRET_ACCESS_KEY"), Source: "env"}
		if keys.AccessKeyID == "" {
			return Keys{}, &Error{Source: "env", Message: "AWS_SECRET_ACCESS_KEY is set without AWS_ACCESS_KEY_ID. Set both, or neither."}
		}
		if keys.SecretAccessKey == "" {
			return Keys{}, &Error{Source: "env", Message: "AWS_ACCESS_KEY_ID is set without AWS_SECRET_ACCESS_KEY. Set both, or neither."}
		}
		for _, name := range []string{"AWS_SECURITY_TOKEN", "AWS_SESSION_TOKEN"} {
			if v, ok := s.opts.Lookup(name); ok {
				keys.SessionToken = v
				break
			}
		}
		if expires := s.env("AWS_CREDENTIAL_EXPIRATION"); expires != "" {
			t, err := parseTime(expires)
			if err != nil {
				return Keys{}, &Error{Source: "env", Message: err.Error()}
			}
			keys.Expires = t
		}
		return keys, nil
	}
	keys, err := read(context.Background())
	if err != nil {
		return nil, err
	}
	if keys.Expires.IsZero() {
		return static{keys}, nil
	}
	return renewing(read, &keys), nil
}

// Keys a section holds, nil without an access key and an error with half a pair
func section(values map[string]string, source string) (Provider, error) {
	key := values["aws_access_key_id"]
	if key == "" {
		return nil, nil
	}
	secret := values["aws_secret_access_key"]
	if secret == "" {
		return nil, &Error{Source: source, Message: "The AWS profile in " + source + " has aws_access_key_id without aws_secret_access_key. Set both."}
	}
	token := values["aws_session_token"]
	if token == "" {
		token = values["aws_security_token"]
	}
	return static{Keys{AccessKeyID: key, SecretAccessKey: secret, SessionToken: token, Source: source}}, nil
}

func pair(p map[string]string) bool {
	return p["aws_access_key_id"] != "" && p["aws_secret_access_key"] != ""
}

// Keys a profile's credential_process prints, its version 1 JSON
func process(ctx context.Context, s *session, profile string) (Provider, error) {
	p, err := s.profile(profile)
	if err != nil {
		return nil, err
	}
	command := p["credential_process"]
	if command == "" {
		return nil, nil
	}
	fetch := func(ctx context.Context) (Keys, error) { return run(ctx, command) }
	keys, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	if keys.Expires.IsZero() {
		return static{keys}, nil
	}
	return renewing(fetch, &keys), nil
}

// Runs a credential_process until it ends or ctx does, the pipes an orphan keeps open closed a second after
func run(ctx context.Context, command string) (Keys, error) {
	argv, err := split(command)
	if err != nil {
		return Keys{}, &Error{Source: "custom-process", Message: err.Error()}
	}
	if len(argv) == 0 {
		return Keys{}, &Error{Source: "custom-process", Message: "credential_process is set but empty in your AWS config."}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		if ctx.Err() != nil {
			return Keys{}, ctx.Err()
		}
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			return Keys{}, &Error{Source: "custom-process", Message: stderr.String()}
		}
		return Keys{}, &Error{Source: "custom-process", Message: err.Error(), Err: err}
	}
	var raw map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return Keys{}, &Error{Source: "custom-process", Message: err.Error(), Err: err}
	}
	if version, ok := raw["Version"].(float64); !ok || version != 1 {
		shown := "none"
		if v, ok := raw["Version"]; ok {
			shown = fmt.Sprint(v)
		}
		return Keys{}, &Error{Source: "custom-process", Message: fmt.Sprintf("The AWS credential_process gave version %s, but only version 1 is understood.", shown)}
	}
	for _, key := range []string{"AccessKeyId", "SecretAccessKey"} {
		if _, ok := raw[key]; !ok {
			return Keys{}, &Error{Source: "custom-process", Message: fmt.Sprintf("The AWS credential_process gave no %s.", key)}
		}
	}
	text := func(key string) string { v, _ := raw[key].(string); return v }
	keys := Keys{AccessKeyID: text("AccessKeyId"), SecretAccessKey: text("SecretAccessKey"), SessionToken: text("SessionToken"), Source: "custom-process"}
	if expires := text("Expiration"); expires != "" {
		t, err := parseTime(expires)
		if err != nil {
			return Keys{}, &Error{Source: "custom-process", Message: err.Error()}
		}
		keys.Expires = t
	}
	return keys, nil
}

// ---- STS ----

type stsKeys struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey"`
	SessionToken    string `xml:"SessionToken"`
	Expiration      string `xml:"Expiration"`
}

// Regions the global STS endpoint answers for under sts_regional_endpoints legacy
var legacyGlobal = map[string]bool{"ap-northeast-1": true, "ap-south-1": true, "ap-southeast-1": true, "ap-southeast-2": true, "aws-global": true,
	"ca-central-1": true, "eu-central-1": true, "eu-north-1": true, "eu-west-1": true, "eu-west-2": true, "eu-west-3": true, "sa-east-1": true,
	"us-east-1": true, "us-east-2": true, "us-west-1": true, "us-west-2": true}

// STS endpoint of the session and the region it signs for
func (s *session) sts() (string, string) {
	region := strings.TrimSpace(s.setting("AWS_DEFAULT_REGION", "region"))
	signing := region
	if signing == "" || signing == "aws-global" {
		signing = "us-east-1"
	}
	if endpoint := s.endpoint("sts"); endpoint != "" {
		return endpoint, signing
	}
	mode := strings.ToLower(strings.TrimSpace(s.setting("AWS_STS_REGIONAL_ENDPOINTS", "sts_regional_endpoints")))
	if region == "" || region == "aws-global" || mode == "legacy" && legacyGlobal[region] {
		return "https://sts.amazonaws.com", signing
	}
	return "https://sts." + region + "." + Suffix(region), signing
}

// Posts one STS action, signed with the keys given unless none are
func (s *session) call(ctx context.Context, action string, params url.Values, signer *Keys) ([]byte, error) {
	endpoint, region := s.sts()
	params.Set("Action", action)
	params.Set("Version", "2011-06-15")
	body := []byte(params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("User-Agent", "IcebathAgent")
	if signer != nil {
		if err := Sign(req, body, *signer, "sts", region, time.Now()); err != nil {
			return nil, err
		}
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var refused struct {
			Error struct {
				Code    string `xml:"Code"`
				Message string `xml:"Message"`
			} `xml:"Error"`
		}
		xml.Unmarshal(data, &refused)
		return nil, &ServiceError{Operation: action, StatusCode: resp.StatusCode, Code: refused.Error.Code, Message: refused.Error.Message}
	}
	return data, nil
}

func fromSTS(c stsKeys, source string) (Keys, error) {
	keys := Keys{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Source: source}
	if c.Expiration != "" {
		t, err := parseTime(c.Expiration)
		if err != nil {
			return Keys{}, err
		}
		keys.Expires = t
	}
	if !keys.Valid() {
		return Keys{}, &Error{Source: source, Message: "AWS STS answered without credentials."}
	}
	return keys, nil
}

func sessionName() string {
	return fmt.Sprintf("botocore-session-%d", time.Now().Unix())
}

// Role profiles, following source_profile chains and refusing a loop
type roles struct {
	s       *session
	visited []string
}

func assumes(p map[string]string) bool {
	_, role := p["role_arn"]
	_, web := p["web_identity_token_file"]
	return role && !web
}

func (r *roles) load(ctx context.Context, profile string) (Provider, error) {
	f, err := r.s.shared()
	if err != nil {
		return nil, err
	}
	if !assumes(f.merged[profile]) {
		return nil, nil
	}
	r.visited = []string{profile}
	return r.assume(ctx, profile)
}

// Keys of the profile's role, assumed with the keys of its source profile or credential source and an MFA code when it names one
func (r *roles) assume(ctx context.Context, profile string) (Provider, error) {
	f, _ := r.s.shared()
	p := f.merged[profile]
	sourceProfile, hasSource := p["source_profile"]
	credentialSource, hasCredential := p["credential_source"]
	switch {
	case hasSource && hasCredential:
		return nil, &Error{Message: fmt.Sprintf("The AWS profile %q sets both source_profile and credential_source. Keep only one.", profile)}
	case !hasSource && !hasCredential:
		return nil, &Error{Source: "assume-role", Message: "The AWS profile sets role_arn without source_profile or credential_source. Add one of them."}
	case hasCredential:
		switch strings.ToLower(credentialSource) {
		case "environment", "ec2instancemetadata", "ecscontainer":
		default:
			return nil, &Error{Message: fmt.Sprintf("The AWS credential_source %q is not one that AWS knows.", credentialSource)}
		}
	default:
		if err := r.check(profile, sourceProfile); err != nil {
			return nil, err
		}
	}
	var source Provider
	var err error
	if hasCredential {
		source, err = r.credentialSource(ctx, credentialSource, profile)
	} else {
		r.visited = append(r.visited, sourceProfile)
		source, err = r.from(ctx, sourceProfile)
	}
	if err != nil {
		return nil, err
	}
	params := url.Values{"RoleArn": {p["role_arn"]}}
	if v, ok := p["external_id"]; ok {
		params.Set("ExternalId", v)
	}
	serial, mfa := p["mfa_serial"]
	if mfa {
		params.Set("SerialNumber", serial)
	}
	if v, ok := p["duration_seconds"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			params.Set("DurationSeconds", strconv.Itoa(n))
		}
	}
	calls := 0
	return renewing(func(ctx context.Context) (Keys, error) {
		if mfa && calls > 0 {
			return Keys{}, &Error{Message: "The AWS credentials can't be renewed without an MFA code."}
		}
		calls++
		keys, err := source.Retrieve(ctx)
		if err != nil {
			return Keys{}, err
		}
		q := url.Values{}
		for k, v := range params {
			q[k] = append([]string(nil), v...)
		}
		q.Set("RoleSessionName", cmp.Or(p["role_session_name"], sessionName()))
		if mfa {
			code, err := r.s.opts.MFA(ctx, fmt.Sprintf("MFA code for %s: ", serial))
			if err != nil {
				return Keys{}, err
			}
			q.Set("TokenCode", code)
		}
		data, err := r.s.call(ctx, "AssumeRole", q, &keys)
		if err != nil {
			return Keys{}, err
		}
		var out struct {
			Result struct{ Credentials stsKeys } `xml:"AssumeRoleResult"`
		}
		if err := xml.Unmarshal(data, &out); err != nil {
			return Keys{}, err
		}
		return fromSTS(out.Result.Credentials, "assume-role")
	}, nil), nil
}

// Refuses a source profile that doesn't exist or leads back to one visited
func (r *roles) check(parent, source string) error {
	f, _ := r.s.shared()
	p, ok := f.merged[source]
	if !ok {
		return &Error{Message: fmt.Sprintf("The AWS profile %q names the source_profile %q, which does not exist.", parent, source)}
	}
	for _, seen := range r.visited {
		if seen == source && (source != parent || !pair(p)) {
			return &Error{Message: fmt.Sprintf("Your AWS profiles name each other in a loop: %s leads back to %s. Make one of them take "+
				"credentials of its own.", strings.Join(r.visited, ", "), source)}
		}
	}
	return nil
}

// Keys of a source profile: its own keys or sources, else a role it assumes in turn
func (r *roles) from(ctx context.Context, profile string) (Provider, error) {
	f, _ := r.s.shared()
	p := f.merged[profile]
	if pair(p) || !assumes(p) {
		for _, l := range profiled(profile, true) {
			found, err := l.load(ctx, r.s)
			if err != nil {
				return nil, err
			}
			if found != nil {
				return found, nil
			}
		}
		return nil, &Error{Message: fmt.Sprintf("The AWS source profile %q has no credentials of its own. Add them to it.", profile)}
	}
	return r.assume(ctx, profile)
}

func (r *roles) credentialSource(ctx context.Context, name, profile string) (Provider, error) {
	var found Provider
	var err error
	switch strings.ToLower(name) {
	case "environment":
		found, err = fromEnv(r.s)
	case "ec2instancemetadata":
		found, err = instance(ctx, r.s)
	case "ecscontainer":
		found, err = container(ctx, r.s)
	}
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, &Error{Source: name, Message: "The credential_source of the AWS profile " + profile + " gave no credentials."}
	}
	return found, nil
}

// Keys of a role assumed with a web identity token, from the environment first then the profile
func webIdentity(s *session, profile string, noEnv bool) (Provider, error) {
	p, err := s.profile(profile)
	if err != nil {
		return nil, err
	}
	get := func(key, variable string) string {
		if !noEnv {
			if v, ok := s.opts.Lookup(variable); ok {
				return v
			}
		}
		return p[key]
	}
	tokenFile := get("web_identity_token_file", "AWS_WEB_IDENTITY_TOKEN_FILE")
	if tokenFile == "" {
		return nil, nil
	}
	role := get("role_arn", "AWS_ROLE_ARN")
	if role == "" {
		return nil, &Error{Message: "The AWS profile or environment takes a role with a web identity but names no role. Set role_arn in the " +
			"profile, or set AWS_ROLE_ARN."}
	}
	name := get("role_session_name", "AWS_ROLE_SESSION_NAME")
	return renewing(func(ctx context.Context) (Keys, error) {
		token, err := os.ReadFile(expand(tokenFile))
		if err != nil {
			return Keys{}, &Error{Source: "assume-role-with-web-identity", Message: err.Error(), Err: err}
		}
		data, err := s.call(ctx, "AssumeRoleWithWebIdentity", url.Values{"RoleArn": {role}, "RoleSessionName": {cmp.Or(name, sessionName())},
			"WebIdentityToken": {string(token)}}, nil)
		if err != nil {
			return Keys{}, err
		}
		var out struct {
			Result struct{ Credentials stsKeys } `xml:"AssumeRoleWithWebIdentityResult"`
		}
		if err := xml.Unmarshal(data, &out); err != nil {
			return Keys{}, err
		}
		return fromSTS(out.Result.Credentials, "assume-role-with-web-identity")
	}, nil), nil
}

// ---- SSO ----

var ssoRequired = []string{"sso_start_url", "sso_region", "sso_role_name", "sso_account_id"}

// An SSO token that aws sso login renews: missing, unreadable, expired or refused
type LoginNeeded struct {
	Profile string
	Err     error
}

func (e *LoginNeeded) Error() string {
	return fmt.Sprintf("The AWS SSO sign-in of the profile %s has expired. Run `aws sso login --profile %s`.", e.Profile, e.Profile)
}

func (e *LoginNeeded) Unwrap() error { return e.Err }

// Role keys of an SSO profile, legacy or of an sso-session
func sso(s *session, profile string) (Provider, error) {
	f, err := s.shared()
	if err != nil {
		return nil, err
	}
	p := f.merged[profile]
	_, role := p["sso_role_name"]
	_, account := p["sso_account_id"]
	if !role && !account {
		return nil, nil
	}
	resolved := clone(p)
	required := ssoRequired
	if name, ok := p["sso_session"]; ok {
		session, ok := f.sso[name]
		if !ok {
			return nil, &Error{Message: fmt.Sprintf("Your AWS config has no sso-session named %q.", name)}
		}
		for k, v := range session {
			if prior, ok := resolved[k]; ok && prior != v {
				return nil, &Error{Message: fmt.Sprintf("Your AWS profile sets %s to %s, but its sso-session sets it to %s. Make them the same.", k, prior, v)}
			}
			resolved[k] = v
		}
		resolved["session_name"] = name
		required = append(required, "session_name")
	}
	var missing []string
	for _, k := range required {
		if _, ok := resolved[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, &Error{Message: fmt.Sprintf("The AWS profile %q uses SSO but lacks %s.", profile, strings.Join(missing, ", "))}
	}
	tokens := &ssoTokens{s: s, profile: profile, start: resolved["sso_start_url"], region: resolved["sso_region"], session: resolved["session_name"]}
	return renewing(func(ctx context.Context) (Keys, error) {
		token, err := tokens.token(ctx)
		if err != nil {
			return Keys{}, err
		}
		return s.roleKeys(ctx, profile, resolved["sso_region"], resolved["sso_account_id"], resolved["sso_role_name"], token)
	}, nil), nil
}

// Token aws sso login keeps in its cache
type ssoToken struct {
	AccessToken           string `json:"accessToken"`
	ExpiresAt             string `json:"expiresAt"`
	RefreshToken          string `json:"refreshToken,omitempty"`
	ClientID              string `json:"clientId,omitempty"`
	ClientSecret          string `json:"clientSecret,omitempty"`
	RegistrationExpiresAt string `json:"registrationExpiresAt,omitempty"`
}

// Cache file of the token, by the sha1 of the session's name or of the start URL of a legacy profile
func cacheFile(start, session string) string {
	sum := sha1.Sum([]byte(cmp.Or(session, start)))
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aws", "sso", "cache", hex.EncodeToString(sum[:])+".json")
}

// Token of one SSO profile, an sso-session one renewed through sso-oidc inside the advisory window
type ssoTokens struct {
	s                               *session
	profile, start, region, session string
}

func (t *ssoTokens) token(ctx context.Context) (string, error) {
	path := cacheFile(t.start, t.session)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", &Error{Message: fmt.Sprintf("There is no AWS SSO sign-in for %s. Run `aws sso login`.", cmp.Or(t.session, t.start)), Err: &LoginNeeded{Profile: t.profile, Err: err}}
	}
	var raw map[string]any
	var tok ssoToken
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", &Error{Message: "The AWS SSO sign-in can't be read. Run `aws sso login`. " + err.Error(), Err: &LoginNeeded{Profile: t.profile, Err: err}}
	}
	json.Unmarshal(data, &tok)
	if tok.AccessToken == "" || tok.ExpiresAt == "" {
		return "", &Error{Message: fmt.Sprintf("The AWS SSO sign-in for %s can't be read. Run `aws sso login`.", t.start), Err: &LoginNeeded{Profile: t.profile}}
	}
	if t.session == "" {
		return tok.AccessToken, nil
	}
	expires, err := parseTime(tok.ExpiresAt)
	if err != nil {
		return "", &Error{Message: "The AWS SSO sign-in can't be read. Run `aws sso login`. " + err.Error(), Err: &LoginNeeded{Profile: t.profile, Err: err}}
	}
	if time.Until(expires) < advisory {
		if fresh, ok := t.refresh(ctx, tok); ok {
			for k, v := range map[string]any{"startUrl": t.start, "region": t.region, "accessToken": fresh.AccessToken, "expiresAt": fresh.ExpiresAt,
				"clientId": tok.ClientID, "clientSecret": tok.ClientSecret, "registrationExpiresAt": tok.RegistrationExpiresAt} {
				raw[k] = v
			}
			if fresh.RefreshToken != "" {
				raw["refreshToken"] = fresh.RefreshToken
			}
			if encoded, err := json.Marshal(raw); err == nil {
				fsx.Write(path, encoded, 0o600)
			}
			tok = fresh
			expires, _ = parseTime(fresh.ExpiresAt)
		}
	}
	if !expires.After(time.Now()) {
		return "", &Error{Source: "sso", Message: "The AWS SSO sign-in has expired and could not be renewed. Run `aws sso login`.", Err: &LoginNeeded{Profile: t.profile}}
	}
	return tok.AccessToken, nil
}

// Renews the cached token with its refresh token at sso-oidc, false when it can't
func (t *ssoTokens) refresh(ctx context.Context, tok ssoToken) (ssoToken, bool) {
	if tok.RefreshToken == "" || tok.ClientID == "" || tok.ClientSecret == "" || tok.RegistrationExpiresAt == "" {
		return ssoToken{}, false
	}
	if registered, err := parseTime(tok.RegistrationExpiresAt); err != nil || !registered.After(time.Now()) {
		return ssoToken{}, false
	}
	endpoint := cmp.Or(t.s.endpoint("sso-oidc"), "https://oidc."+t.region+"."+Suffix(t.region))
	body, _ := json.Marshal(map[string]string{"grantType": "refresh_token", "clientId": tok.ClientID, "clientSecret": tok.ClientSecret, "refreshToken": tok.RefreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/token", bytes.NewReader(body))
	if err != nil {
		return ssoToken{}, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.s.http.Do(req)
	if err != nil {
		return ssoToken{}, false
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken  string  `json:"accessToken"`
		ExpiresIn    float64 `json:"expiresIn"`
		RefreshToken string  `json:"refreshToken"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil || out.AccessToken == "" {
		return ssoToken{}, false
	}
	return ssoToken{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken,
		ExpiresAt: time.Now().Add(time.Duration(out.ExpiresIn * float64(time.Second))).UTC().Format(time.RFC3339)}, true
}

// Role keys the SSO portal gives the token for one account and role
func (s *session) roleKeys(ctx context.Context, profile, region, account, role, token string) (Keys, error) {
	endpoint := cmp.Or(s.endpoint("sso"), "https://portal.sso."+region+"."+Suffix(region))
	q := url.Values{"account_id": {account}, "role_name": {role}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/federation/credentials?"+q.Encode(), nil)
	if err != nil {
		return Keys{}, err
	}
	req.Header.Set("x-amz-sso_bearer_token", token)
	resp, err := s.http.Do(req)
	if err != nil {
		return Keys{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return Keys{}, &Error{Source: "sso", Message: "The AWS SSO sign-in of this profile has expired or no longer works. Run `aws sso login " +
			"--profile " + profile + "`.", Err: &LoginNeeded{Profile: profile}}
	}
	if resp.StatusCode != http.StatusOK {
		var refused struct {
			Message string `json:"message"`
		}
		json.Unmarshal(data, &refused)
		code, _, _ := strings.Cut(resp.Header.Get("x-amzn-ErrorType"), ":")
		return Keys{}, &ServiceError{Operation: "GetRoleCredentials", StatusCode: resp.StatusCode, Code: strings.TrimSpace(code), Message: refused.Message}
	}
	var out struct {
		Role struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
			Expiration      int64  `json:"expiration"`
		} `json:"roleCredentials"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Keys{}, err
	}
	return Keys{AccessKeyID: out.Role.AccessKeyID, SecretAccessKey: out.Role.SecretAccessKey, SessionToken: out.Role.SessionToken,
		Expires: time.UnixMilli(out.Role.Expiration).UTC(), Source: "sso"}, nil
}

// ---- container and instance roles ----

var containerHosts = map[string]bool{"169.254.170.2": true, "169.254.170.23": true, "fd00:ec2::23": true, "localhost": true}

// Keys of the ECS or EKS container's role, from its relative or full URI with its authorization token
func container(ctx context.Context, s *session) (Provider, error) {
	if !s.has("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") && !s.has("AWS_CONTAINER_CREDENTIALS_FULL_URI") {
		return nil, nil
	}
	full := ""
	if relative, ok := s.opts.Lookup("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"); ok {
		full = "http://169.254.170.2" + relative
	} else {
		full = s.env("AWS_CONTAINER_CREDENTIALS_FULL_URI")
		u, err := url.Parse(full)
		if err != nil {
			return nil, &Error{Source: "container-role", Message: err.Error(), Err: err}
		}
		h := u.Hostname()
		if ip := net.ParseIP(h); !(ip != nil && ip.IsLoopback()) && !containerHosts[h] {
			return nil, &Error{Source: "container-role", Message: fmt.Sprintf("AWS credentials are read only from this computer or from "+
				"169.254.170.2, 169.254.170.23, fd00:ec2::23 or localhost, not from %s.", h)}
		}
	}
	fetch := func(ctx context.Context) (Keys, error) {
		headers := map[string]string{"Accept": "application/json"}
		token := ""
		if path, ok := s.opts.Lookup("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); ok {
			data, err := os.ReadFile(path)
			if err != nil {
				return Keys{}, &Error{Source: "container-role", Message: err.Error(), Err: err}
			}
			token = string(data)
		} else if v, ok := s.opts.Lookup("AWS_CONTAINER_AUTHORIZATION_TOKEN"); ok {
			token = v
		}
		if token != "" || s.has("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE") || s.has("AWS_CONTAINER_AUTHORIZATION_TOKEN") {
			if strings.ContainsAny(token, "\r\n") {
				return Keys{}, &Error{Source: "container-role", Message: "The container's AWS token can't be sent, because it holds characters a request can't carry."}
			}
			headers["Authorization"] = token
		}
		var last error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return Keys{}, ctx.Err()
				case <-time.After(time.Second):
				}
			}
			keys, err := containerOnce(ctx, s.http, full, headers)
			if err == nil {
				return keys, nil
			}
			last = err
		}
		return Keys{}, &Error{Source: "container-role", Message: last.Error(), Err: last}
	}
	keys, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	return renewing(fetch, &keys), nil
}

func containerOnce(ctx context.Context, client *http.Client, full string, headers map[string]string) (Keys, error) {
	capped, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(capped, http.MethodGet, full, nil)
	if err != nil {
		return Keys{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Keys{}, fmt.Errorf("AWS credentials could not be read from the container's metadata service: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Keys{}, fmt.Errorf("The container's metadata service answered with HTTP status %d instead of AWS credentials: %s", resp.StatusCode, string(data))
	}
	var out struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		Token           string `json:"Token"`
		Expiration      string `json:"Expiration"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Keys{}, errors.New("The container's metadata service gave AWS credentials that can't be read.")
	}
	keys := Keys{AccessKeyID: out.AccessKeyID, SecretAccessKey: out.SecretAccessKey, SessionToken: out.Token, Source: "container-role"}
	if out.Expiration != "" {
		t, err := parseTime(out.Expiration)
		if err != nil {
			return Keys{}, err
		}
		keys.Expires = t
	}
	return keys, nil
}

// Instance metadata service of the session: its endpoint, timeout, attempts and whether it or its version 1 is off
type imds struct {
	endpoint   string
	timeout    time.Duration
	attempts   int
	off, v1Off bool
}

func (s *session) imds() imds {
	c := imds{endpoint: "http://169.254.169.254/", timeout: time.Second, attempts: 1}
	if v := s.setting("AWS_EC2_METADATA_SERVICE_ENDPOINT", "ec2_metadata_service_endpoint"); v != "" {
		c.endpoint = strings.TrimRight(v, "/") + "/"
	} else if strings.EqualFold(s.setting("AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE", "ec2_metadata_service_endpoint_mode"), "ipv6") {
		c.endpoint = "http://[fd00:ec2::254]/"
	}
	if f, err := strconv.ParseFloat(s.setting("AWS_METADATA_SERVICE_TIMEOUT", "metadata_service_timeout"), 64); err == nil && f > 0 {
		c.timeout = time.Duration(f * float64(time.Second))
	}
	if n, err := strconv.Atoi(s.setting("AWS_METADATA_SERVICE_NUM_ATTEMPTS", "metadata_service_num_attempts")); err == nil && n > 0 {
		c.attempts = n
	}
	c.off = s.opts.NoIMDS || truthy(s.setting("AWS_EC2_METADATA_DISABLED", "ec2_metadata_disabled"))
	c.v1Off = truthy(s.setting("AWS_EC2_METADATA_V1_DISABLED", "ec2_metadata_v1_disabled"))
	return c
}

// Keys of the instance's role over IMDSv2, nil when metadata is off or answers nothing usable
func instance(ctx context.Context, s *session) (Provider, error) {
	cfg := s.imds()
	if cfg.off {
		return nil, nil
	}
	keys, ok := cfg.fetch(ctx, s.http)
	if !ok {
		return nil, nil
	}
	return renewing(func(ctx context.Context) (Keys, error) {
		keys, ok := cfg.fetch(ctx, s.http)
		if !ok {
			return Keys{}, &Error{Source: "iam-role", Message: "The instance metadata service gave no AWS credentials."}
		}
		return keys, nil
	}, &keys), nil
}

// Session token of IMDSv2 with a six hour lifetime; none on 403, 404 and 405, where version 1 may still answer
func (c imds) token(ctx context.Context, client *http.Client) (string, bool, error) {
	for i := 0; i < c.attempts; i++ {
		capped, cancel := context.WithTimeout(ctx, c.timeout)
		req, err := http.NewRequestWithContext(capped, http.MethodPut, c.endpoint+"latest/api/token", nil)
		if err != nil {
			cancel()
			return "", false, err
		}
		req.Header.Set("x-aws-ec2-metadata-token-ttl-seconds", "21600")
		req.Header.Set("User-Agent", "IcebathAgent")
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			if network, ok := errors.AsType[net.Error](err); ok && network.Timeout() {
				return "", false, nil
			}
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		cancel()
		switch resp.StatusCode {
		case http.StatusOK:
			return string(data), true, nil
		case http.StatusNotFound, http.StatusForbidden, http.StatusMethodNotAllowed:
			return "", false, nil
		case http.StatusBadRequest:
			return "", false, errors.New("The instance metadata service refused the request for AWS credentials.")
		}
	}
	return "", false, nil
}

func (c imds) get(ctx context.Context, client *http.Client, path, token string, ok func(int, []byte) bool) ([]byte, bool) {
	for i := 0; i < c.attempts; i++ {
		capped, cancel := context.WithTimeout(ctx, c.timeout)
		req, err := http.NewRequestWithContext(capped, http.MethodGet, c.endpoint+path, nil)
		if err != nil {
			cancel()
			return nil, false
		}
		req.Header.Set("User-Agent", "IcebathAgent")
		if token != "" {
			req.Header.Set("x-aws-ec2-metadata-token", token)
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		cancel()
		if ok(resp.StatusCode, data) {
			return data, true
		}
	}
	return nil, false
}

// The role's name then its keys, an expiry inside the renewal window pushed out a little so a stale instance keeps working
func (c imds) fetch(ctx context.Context, client *http.Client) (Keys, bool) {
	token, haveToken, err := c.token(ctx, client)
	if err != nil || !haveToken && c.v1Off {
		return Keys{}, false
	}
	const base = "latest/meta-data/iam/security-credentials/"
	roleData, ok := c.get(ctx, client, base, token, func(status int, data []byte) bool { return status == 200 && len(data) > 0 })
	if !ok {
		return Keys{}, false
	}
	data, ok := c.get(ctx, client, base+strings.TrimSpace(string(roleData)), token, func(status int, data []byte) bool {
		var probe map[string]any
		return status == 200 && len(data) > 0 && json.Unmarshal(data, &probe) == nil
	})
	if !ok {
		return Keys{}, false
	}
	var out map[string]any
	json.Unmarshal(data, &out)
	text := func(key string) string { v, _ := out[key].(string); return v }
	for _, key := range []string{"AccessKeyId", "SecretAccessKey", "Token", "Expiration"} {
		if _, has := out[key]; !has {
			return Keys{}, false
		}
	}
	keys := Keys{AccessKeyID: text("AccessKeyId"), SecretAccessKey: text("SecretAccessKey"), SessionToken: text("Token"), Source: "iam-role"}
	if expires, err := parseTime(text("Expiration")); err == nil {
		offset := 10*time.Minute + time.Duration(120+rand.IntN(481))*time.Second
		if now := time.Now().UTC(); !now.Before(expires.Add(-offset)) {
			expires = now.Add(offset)
		}
		keys.Expires = expires
	}
	return keys, true
}

// ---- probes ----

// Variables whose presence says which AWS sign-in is in use, in the order they are looked at
var markers = [][]string{{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}, {"AWS_PROFILE"}, {"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"}, {"AWS_WEB_IDENTITY_TOKEN_FILE"}}

// Name of the AWS sign-in in use: the first variable set of its kind, else iam-role when the chain answers, empty when nothing does
func Detect(ctx context.Context, lookup func(string) (string, bool), instanceMetadata bool) string {
	for _, group := range markers {
		set := true
		for _, name := range group {
			if v, _ := lookup(name); strings.TrimSpace(v) == "" {
				set = false
				break
			}
		}
		if set {
			return group[0]
		}
	}
	chain := NewChain(Options{NoIMDS: !instanceMetadata, Lookup: lookup, MFA: func(context.Context, string) (string, error) {
		return "", errors.New("This AWS profile needs an MFA code, which icebath doesn't ask for while it looks for credentials. Sign in with `aws sts get-session-token` first.")
	}})
	if keys, err := chain.Retrieve(ctx); err == nil && keys.AccessKeyID != "" {
		return "iam-role"
	}
	return ""
}

// ARN of the principal the keys sign as, from STS GetCallerIdentity in the region
func Identity(ctx context.Context, keys Keys, region string) (string, error) {
	body := []byte("Action=GetCallerIdentity&Version=2011-06-15")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://sts.%s.%s/", region, Suffix(region)), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	if err := Sign(req, body, keys, "sts", region, time.Now()); err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("AWS could not say who these credentials belong to, and answered with HTTP status %d.", resp.StatusCode)
	}
	var parsed struct {
		Arn string `xml:"GetCallerIdentityResult>Arn"`
	}
	if err := xml.Unmarshal(data, &parsed); err != nil {
		return "", err
	}
	return cmp.Or(parsed.Arn, "unknown"), nil
}
