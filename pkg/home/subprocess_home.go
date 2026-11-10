package home

import (
	"context"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var homeModeAliases = map[string]string{
	"isolated": "profile", "profile_home": "profile", "profile-home": "profile",
	"host": "real", "user": "real", "real_home": "real", "real-home": "real",
}

// Comparable absolute and case normalized form of a home path, empty for blank input
func NormHomePath(path string) string {
	raw := pystr.Strip(path)
	if raw == "" {
		return ""
	}
	abs, err := pypath.Abspath(pypath.Expanduser(raw))
	if err != nil {
		return pypath.Normcase(raw)
	}
	return pypath.Normcase(abs)
}

// The home subdirectory of the home in effect when that directory exists
func profileHomePath(ctx context.Context, env map[string]string) string {
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
	p := pypath.PathJoin(expand(h), "home")
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p
	}
	return ""
}

func isProfileHome(candidate, profileHome string) bool {
	return candidate != "" && profileHome != "" && NormHomePath(candidate) == NormHomePath(profileHome)
}

// Stripped key from env, falling back to the process environment and then def
func envGet(env map[string]string, key, def string) string {
	if v := env[key]; v != "" {
		return pystr.Strip(v)
	}
	if v, ok := os.LookupEnv(key); ok {
		return pystr.Strip(v)
	}
	return pystr.Strip(def)
}

// Likely OS user home candidates in trust order
func realHomeCandidates(env map[string]string) []string {
	candidates := []string{envGet(env, "ICEBATH_REAL_HOME", ""), envGet(env, "HOME", "")}
	if runtime.GOOS != "windows" {
		if u, err := user.LookupId(strconv.Itoa(os.Getuid())); err == nil {
			candidates = append(candidates, pystr.Strip(u.HomeDir))
		}
	}
	candidates = append(candidates, envGet(env, "USERPROFILE", ""))
	drive, path := envGet(env, "HOMEDRIVE", ""), envGet(env, "HOMEPATH", "")
	if drive != "" && path != "" {
		if strings.HasPrefix(path, `\`) || strings.HasPrefix(path, "/") {
			candidates = append(candidates, drive+path)
		} else {
			candidates = append(candidates, pypath.Join(drive, path))
		}
	}
	if expanded := pypath.Expanduser("~"); expanded != "~" {
		candidates = append(candidates, expanded)
	}
	out := candidates[:0]
	for _, c := range candidates {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// The OS user's real home, never the profile home, the temp root when no candidate fits
func RealHome(ctx context.Context, env map[string]string) string {
	profile := profileHomePath(ctx, env)
	seen := map[string]bool{}
	for _, candidate := range realHomeCandidates(env) {
		key := NormHomePath(candidate)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if !isProfileHome(candidate, profile) {
			return candidate
		}
	}
	if tmp, err := pypath.Gettempdir(); err == nil {
		return tmp
	}
	return "/tmp"
}

// HOME a subprocess gets under TERMINAL_HOME_MODE, empty when it keeps its own
func SubprocessHome(ctx context.Context, env map[string]string) string {
	profile := profileHomePath(ctx, env)
	mode := pystr.Lower(envGet(env, "TERMINAL_HOME_MODE", "auto"))
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
	current := envGet(env, "HOME", "")
	repaired := ""
	if NormHomePath(real) != NormHomePath(current) {
		repaired = real
	}
	if mode == "real" {
		return repaired
	}
	if profile != "" && isContainer() {
		return profile
	}
	if current == "" || isProfileHome(current, profile) {
		return repaired
	}
	return ""
}

// Sets HOME and ICEBATH_REAL_HOME per the home mode and points the temp variables at the scratch dir
func ApplySubprocessHomeEnv(ctx context.Context, env map[string]string) {
	if real := RealHome(ctx, env); real != "" {
		env["ICEBATH_REAL_HOME"] = real
	}
	if h := SubprocessHome(ctx, env); h != "" {
		env["HOME"] = h
	}
	ApplyScratchTmpEnv(ctx, env)
}
