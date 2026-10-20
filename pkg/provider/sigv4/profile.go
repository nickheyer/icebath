package sigv4

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
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
