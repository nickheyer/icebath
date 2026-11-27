package git

import (
	"context"
	"encoding/base64"
	"maps"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Hosts whose credentials a GitHub token is
var githubHosts = map[string]bool{"github.com": true, "www.github.com": true, "gist.github.com": true}

// What git says when the remote asks for a credential it could not give, rather than failing for another reason
var credentialRequired = regexp.MustCompile(`(?i)could not read (?:username|password)|terminal prompts disabled|authentication failed|` +
	`(?:error|status|http)[: ]+40[13]\b`)

// Username and password git sends as a basic Authorization header
type basicAuth struct{ username, password string }

// Origin of an https address, false for any other
func httpsOrigin(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return "", false
	}
	return "https://" + parsed.Host, true
}

// Token of the gh command's own login, the tokens the environment exports hidden from it; empty when gh is not there or has none
func ghToken(ctx context.Context, base map[string]string) string {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	env := Env(ctx, base)
	env["GH_PROMPT_DISABLED"] = "1"
	delete(env, "GH_TOKEN")
	delete(env, "GITHUB_TOKEN")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, "auth", "token")
	cmd.Env = Environ(env)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Username and password the user's credential helpers keep for the origin, never prompting
func fill(ctx context.Context, origin string) (basicAuth, bool) {
	parsed, err := url.Parse(origin)
	if err != nil {
		return basicAuth{}, false
	}
	env := Process()
	env["GIT_TERMINAL_PROMPT"], env["GCM_INTERACTIVE"] = "0", "Never"
	delete(env, "GIT_ASKPASS")
	delete(env, "SSH_ASKPASS")
	result, err := Run(ctx, "", env, 15*time.Second, []byte("protocol=https\nhost="+parsed.Host+"\n\n"), "-c", "core.askPass=", "credential", "fill")
	if err != nil || result.Code != 0 {
		return basicAuth{}, false
	}
	fields := map[string]string{}
	for _, line := range Lines(result.Stdout) {
		if key, value, ok := strings.Cut(line, "="); ok {
			fields[key] = value
		}
	}
	if fields["password"] == "" {
		return basicAuth{}, false
	}
	return basicAuth{fields["username"], fields["password"]}, true
}

// Runs a network verb of git, such as a clone, anonymously first, then with each credential the user has for the address's host while the
// remote keeps asking for one: a GitHub token of the environment lookup reads, the gh command's login, then what the credential helpers
// keep. A non zero exit is a Result, as Run's is
func Fetch(ctx context.Context, dir string, timeout time.Duration, address string, lookup func(name string) string, args ...string) (*Result, error) {
	env := Env(ctx, nil)
	delete(env, "GIT_ASKPASS")
	delete(env, "SSH_ASKPASS")
	result, err := Run(ctx, dir, env, timeout, nil, args...)
	if err != nil || result.Code == 0 || !credentialRequired.MatchString(result.Stderr+"\n"+result.Stdout) {
		return result, err
	}
	origin, ok := httpsOrigin(address)
	if !ok {
		return result, nil
	}
	// The token of the environment, which the hint names when the remote refuses it
	const envToken = "env"
	type candidate struct {
		source string
		find   func() (basicAuth, bool)
	}
	var candidates []candidate
	if parsed, _ := url.Parse(origin); githubHosts[parsed.Hostname()] {
		token := func(value string) (basicAuth, bool) { return basicAuth{"x-access-token", value}, value != "" }
		candidates = append(candidates,
			candidate{envToken, func() (basicAuth, bool) {
				for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
					if value := strings.TrimSpace(lookup(name)); value != "" {
						return token(value)
					}
				}
				return basicAuth{}, false
			}},
			candidate{"gh", func() (basicAuth, bool) { return token(ghToken(ctx, nil)) }})
	}
	candidates = append(candidates, candidate{"helper", func() (basicAuth, bool) { return fill(ctx, origin) }})
	tried := map[basicAuth]bool{}
	refusedEnv := false
	for _, c := range candidates {
		auth, ok := c.find()
		if !ok || tried[auth] {
			continue
		}
		tried[auth] = true
		authed := maps.Clone(env)
		Configure(authed, [2]string{"http." + origin + "/.extraheader",
			"Authorization: basic " + base64.StdEncoding.EncodeToString([]byte(auth.username+":"+auth.password))})
		if result, err = Run(ctx, dir, authed, timeout, nil, args...); err != nil || result.Code == 0 ||
			!credentialRequired.MatchString(result.Stderr+"\n"+result.Stdout) {
			return result, err
		}
		refusedEnv = refusedEnv || c.source == envToken
	}
	if refusedEnv {
		result.Stderr += "\nhint: the GITHUB_TOKEN/GH_TOKEN in your .env was rejected by GitHub; replace it or remove it (gh auth login is used when it " +
			"is absent)."
	}
	return result, nil
}
