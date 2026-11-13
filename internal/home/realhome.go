package home

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Subprocess HOME modes TERMINAL_HOME_MODE accepts and the alias spellings each folds from
var homeModeAliases = map[string]string{
	"isolated": "profile", "profile_home": "profile", "profile-home": "profile",
	"host": "real", "user": "real", "real_home": "real", "real-home": "real",
}

// Comparable absolute form of a home path, empty for blank input
func normHomePath(path string) string {
	raw := strings.TrimSpace(path)
	if raw == "" {
		return ""
	}
	abs, err := filepath.Abs(ExpandUser(raw))
	if err != nil {
		return normCase(raw)
	}
	return normCase(abs)
}

// Stripped value of key in env, the process environment when env leaves it empty
func envGet(env map[string]string, key string) string {
	if v := env[key]; v != "" {
		return strings.TrimSpace(v)
	}
	return envValue(key)
}

// Subprocess HOME directory below the override or ICEBATH_HOME in effect, empty when unset or missing
func ProfileHomeDir(ctx context.Context, env map[string]string) string {
	h := Override(ctx)
	if h == "" {
		h = env[EnvHome]
	}
	if h == "" {
		h = os.Getenv(EnvHome)
	}
	if h == "" {
		return ""
	}
	if p := SubprocessHomeDir.In(expandHome(h)); isDir(p) {
		return p
	}
	return ""
}

// Reports whether candidate names the profile home directory
func isProfileHome(candidate, profileHome string) bool {
	return candidate != "" && profileHome != "" && normHomePath(candidate) == normHomePath(profileHome)
}

// Likely account home candidates in trust order
func realHomeCandidates(env map[string]string) []string {
	candidates := []string{envGet(env, EnvRealHome), envGet(env, "HOME")}
	if h, ok := accountDatabaseHome(); ok {
		candidates = append(candidates, strings.TrimSpace(h))
	}
	candidates = append(candidates, envGet(env, "USERPROFILE"))
	if drive, path := envGet(env, "HOMEDRIVE"), envGet(env, "HOMEPATH"); drive != "" && path != "" {
		if strings.HasPrefix(path, `\`) || strings.HasPrefix(path, "/") {
			candidates = append(candidates, drive+path)
		} else {
			candidates = append(candidates, filepath.Join(drive, path))
		}
	}
	if h := ExpandUser("~"); h != "~" {
		candidates = append(candidates, h)
	}
	out := candidates[:0]
	for _, c := range candidates {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// Account home of the real user, never the profile home directory, a usable temp root when no candidate fits
func RealHome(ctx context.Context, env map[string]string) string {
	profile := ProfileHomeDir(ctx, env)
	seen := map[string]bool{}
	for _, c := range realHomeCandidates(env) {
		key := normHomePath(c)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if !isProfileHome(c, profile) {
			return c
		}
	}
	if dir, err := usableTempDir(); err == nil {
		return dir
	}
	return "/tmp"
}

// HOME a subprocess receives under TERMINAL_HOME_MODE, empty when it keeps the one it has
func (r *Resolver) SubprocessHome(ctx context.Context, env map[string]string) string {
	profile := ProfileHomeDir(ctx, env)
	mode := strings.ToLower(envGet(env, "TERMINAL_HOME_MODE"))
	if mode == "" {
		mode = "auto"
	}
	if alias, ok := homeModeAliases[mode]; ok {
		mode = alias
	}
	if mode == "profile" {
		return profile
	}
	real := RealHome(ctx, env)
	current := envGet(env, "HOME")
	repaired := ""
	if normHomePath(real) != normHomePath(current) {
		repaired = real
	}
	if mode == "real" {
		return repaired
	}
	if profile != "" && r.host.Container().Inside() {
		return profile
	}
	if current == "" || isProfileHome(current, profile) {
		return repaired
	}
	return ""
}

// Sets ICEBATH_REAL_HOME and HOME in env per the home mode and points its temp variables at its home scratch directory
func (r *Resolver) ApplySubprocessHomeEnv(ctx context.Context, env map[string]string) {
	if real := RealHome(ctx, env); real != "" {
		env[EnvRealHome] = real
	}
	if h := r.SubprocessHome(ctx, env); h != "" {
		env["HOME"] = h
	}
	r.ApplyScratchTmpEnv(env)
}

// Platform default home of the OS account rather than of a foreign HOME, the process default when the two agree
func AccountDefault(ctx context.Context) (string, error) {
	account, ok := accountDatabaseHome()
	if !ok {
		account = RealHome(ctx, nil)
	}
	account = filepath.Clean(account)
	if same, err := sameResolved(account); err == nil && same {
		return PlatformDefault()
	}
	return platformDefaultFrom(os.Getenv, func() (string, error) { return account, nil })
}

// Reports whether the process account home and account resolve to one directory
func sameResolved(account string) (bool, error) {
	h, err := UserHome()
	if err != nil {
		return false, err
	}
	a, err := resolvePath(h)
	if err != nil {
		return false, err
	}
	b, err := resolvePath(account)
	return err == nil && samePath(a, b), err
}
