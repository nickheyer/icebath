package aws

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/proc"
)

const processTimeout = time.Minute

func readEnvCredentials(requireExpiry bool) (Credentials, error) {
	creds := Credentials{AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), Source: "env"}
	if creds.AccessKeyID == "" {
		return Credentials{}, partial("env", "AWS_ACCESS_KEY_ID")
	}
	if creds.SecretAccessKey == "" {
		return Credentials{}, partial("env", "AWS_SECRET_ACCESS_KEY")
	}
	for _, name := range []string{"AWS_SECURITY_TOKEN", "AWS_SESSION_TOKEN"} {
		if v := os.Getenv(name); v != "" {
			creds.SessionToken = v
			break
		}
	}
	expiry := os.Getenv("AWS_CREDENTIAL_EXPIRATION")
	if expiry == "" {
		if requireExpiry {
			return Credentials{}, partial("env", "AWS_CREDENTIAL_EXPIRATION")
		}
		return creds, nil
	}
	t, err := parseTime(expiry)
	if err != nil {
		return Credentials{}, &Error{Source: "env", Message: "AWS_CREDENTIAL_EXPIRATION is not a timestamp", Err: err}
	}
	creds.Expires = t
	return creds, nil
}

func envProvider() (provider, error) {
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		return nil, nil
	}
	creds, err := readEnvCredentials(false)
	if err != nil {
		return nil, err
	}
	if creds.Expires.IsZero() {
		return static{creds}, nil
	}
	return newRefreshing(func(context.Context) (Credentials, error) { return readEnvCredentials(true) }, &creds), nil
}

func staticFromSection(sec section, source string) (provider, error) {
	key, ok := sec["aws_access_key_id"]
	if !ok {
		return nil, nil
	}
	secret, ok := sec["aws_secret_access_key"]
	if !ok {
		return nil, partial(source, "aws_secret_access_key")
	}
	creds := Credentials{AccessKeyID: key, SecretAccessKey: secret, Source: source}
	for _, name := range []string{"aws_security_token", "aws_session_token"} {
		if v, ok := sec[name]; ok {
			creds.SessionToken = v
			break
		}
	}
	return static{creds}, nil
}

func hasStaticKeys(p section) bool {
	_, key := p["aws_access_key_id"]
	_, secret := p["aws_secret_access_key"]
	return key || secret
}

func (r *resolver) process(ctx context.Context, profile string) (provider, error) {
	command, ok := r.s.files.profiles[profile]["credential_process"]
	if !ok {
		return nil, nil
	}
	fetch := func(ctx context.Context) (Credentials, error) { return runCredentialProcess(ctx, command) }
	creds, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	if creds.Expires.IsZero() {
		return static{creds}, nil
	}
	return newRefreshing(fetch, &creds), nil
}

func runCredentialProcess(ctx context.Context, command string) (Credentials, error) {
	const source = "custom-process"
	argv, err := shellSplit(command)
	if err != nil {
		return Credentials{}, &Error{Source: source, Message: "credential_process is not a readable command line", Err: err}
	}
	if len(argv) == 0 {
		return Credentials{}, &Error{Source: source, Message: "credential_process is empty"}
	}
	ctx, cancel := context.WithTimeout(ctx, processTimeout)
	defer cancel()
	cmd := exec.Command(argv[0], argv[1:]...)
	proc.Hide(cmd)
	res, err := proc.Run(ctx, cmd)
	if err != nil {
		if ctx.Err() != nil {
			return Credentials{}, &Error{Source: source, Message: "credential_process did not finish within " + processTimeout.String(), Err: ctx.Err()}
		}
		return Credentials{}, &Error{Source: source, Message: "credential_process could not be run", Err: err}
	}
	if !res.Exit.Success() {
		text := strings.TrimSpace(string(res.Stderr))
		if len(text) > stderrLimit {
			text = text[:stderrLimit]
		}
		return Credentials{}, &Error{Source: source, Message: "credential_process failed (" + res.Exit.String() + "): " + text}
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(res.Stdout, &parsed); err != nil {
		return Credentials{}, &Error{Source: source, Message: "credential_process did not print a JSON object"}
	}
	var version float64
	if raw, ok := parsed["Version"]; !ok || json.Unmarshal(raw, &version) != nil || version != 1 {
		return Credentials{}, &Error{Source: source, Message: "unsupported version for credential process provider, supported versions: 1"}
	}
	creds := Credentials{Source: source}
	for _, field := range []struct {
		key      string
		dst      *string
		required bool
	}{{"AccessKeyId", &creds.AccessKeyID, true}, {"SecretAccessKey", &creds.SecretAccessKey, true}, {"SessionToken", &creds.SessionToken, false}} {
		raw, ok := parsed[field.key]
		if !ok {
			if field.required {
				return Credentials{}, &Error{Source: source, Message: "missing required key in response: " + field.key}
			}
			continue
		}
		if string(raw) == "null" && !field.required {
			continue
		}
		if err := json.Unmarshal(raw, field.dst); err != nil {
			return Credentials{}, &Error{Source: source, Message: "response key " + field.key + " is not a string"}
		}
	}
	if raw, ok := parsed["Expiration"]; ok && string(raw) != "null" {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return Credentials{}, &Error{Source: source, Message: "response key Expiration is not a string"}
		}
		t, err := parseTime(text)
		if err != nil {
			return Credentials{}, &Error{Source: source, Message: "response key Expiration is not a timestamp", Err: err}
		}
		creds.Expires = t
	}
	return creds, nil
}

func ec2FileProvider() (provider, error) {
	const source = "ec2-credentials-file"
	path, ok := os.LookupEnv("AWS_CREDENTIAL_FILE")
	if !ok {
		return nil, nil
	}
	path = home.ExpandUser(path)
	data, err := readBounded(path)
	if err != nil {
		return nil, &Error{Source: source, Message: "unable to read " + path, Err: err}
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	key, ok := values["AWSAccessKeyId"]
	if !ok {
		return nil, nil
	}
	secret, ok := values["AWSSecretKey"]
	if !ok {
		return nil, partial(source, "AWSSecretKey")
	}
	return static{Credentials{AccessKeyID: key, SecretAccessKey: secret, Source: source}}, nil
}

func botoProvider() (provider, error) {
	const source = "boto-config"
	locations := []string{"/etc/boto.cfg", "~/.boto"}
	if v, ok := os.LookupEnv("BOTO_CONFIG"); ok {
		locations = []string{v}
	}
	for _, location := range locations {
		doc, found, err := readINI(expandPath(location))
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		sec, ok := doc.flat["Credentials"]
		if !ok {
			continue
		}
		key, ok := sec["aws_access_key_id"]
		if !ok {
			continue
		}
		secret, ok := sec["aws_secret_access_key"]
		if !ok {
			return nil, partial(source, "aws_secret_access_key")
		}
		return static{Credentials{AccessKeyID: key, SecretAccessKey: secret, Source: source}}, nil
	}
	return nil, nil
}

func readBounded(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() > replyLimit {
		return nil, errors.New("file is larger than 1 MiB")
	}
	return os.ReadFile(path)
}

func posixSplit(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			if i+1 >= len(s) {
				return nil, errors.New("no escaped character")
			}
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("no closing quotation")
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
				return nil, errors.New("no closing quotation")
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
