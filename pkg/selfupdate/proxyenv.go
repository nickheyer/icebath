package selfupdate

import (
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	ProxyEnvMarker      = "ICEBATH_UPDATE_SYSTEM_PROXY"
	ProxyEnvSavedPrefix = "ICEBATH_UPDATE_USER_"
)

var proxyAddedKeys = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"}

var proxyNoProxyKeys = []string{"NO_PROXY", "no_proxy"}

// Values the last dotenv loads published under a name, swapped by tests
var dotenvPublishedValues = provider.DotenvPublishedValues

// Mutable environment in iteration order, the process environment when built from nil
type proxyEnv struct {
	keys    []string
	vals    map[string]string
	process bool
}

func newProxyEnv(environ map[string]string) *proxyEnv {
	env := &proxyEnv{vals: map[string]string{}}
	if environ == nil {
		env.process = true
		for _, entry := range os.Environ() {
			if key, value, ok := strings.Cut(entry, "="); ok {
				if _, seen := env.vals[key]; !seen {
					env.keys = append(env.keys, key)
				}
				env.vals[key] = value
			}
		}
		return env
	}
	for key, value := range environ {
		env.keys = append(env.keys, key)
		env.vals[key] = value
	}
	sort.Strings(env.keys)
	return env
}

func (e *proxyEnv) get(key string) (string, bool) {
	value, ok := e.vals[key]
	return value, ok
}

func (e *proxyEnv) set(key, value string, environ map[string]string) {
	if _, ok := e.vals[key]; !ok {
		e.keys = append(e.keys, key)
	}
	e.vals[key] = value
	if e.process {
		os.Setenv(key, value)
	} else {
		environ[key] = value
	}
}

func (e *proxyEnv) pop(key string, environ map[string]string) (string, bool) {
	value, ok := e.vals[key]
	if !ok {
		return "", false
	}
	delete(e.vals, key)
	e.keys = slices.DeleteFunc(e.keys, func(k string) bool { return k == key })
	if e.process {
		os.Unsetenv(key)
	} else {
		delete(environ, key)
	}
	return value, true
}

// Drops the proxy variables the desktop added and restores the user's own NO_PROXY, nil meaning the process env
func RestoreUserProxyEnv(environ map[string]string) {
	restoreUserProxyEnv(newProxyEnv(environ), environ)
}

func restoreUserProxyEnv(env *proxyEnv, environ map[string]string) {
	usersDotenvValue := func(key string) bool {
		value, ok := env.get(key)
		return ok && slices.Contains(dotenvPublishedValues(key), value)
	}
	marker, _ := env.get(ProxyEnvMarker)
	for _, key := range strings.Split(marker, ",") {
		if slices.Contains(proxyAddedKeys, key) && !usersDotenvValue(key) {
			env.pop(key, environ)
		}
	}
	for _, key := range proxyNoProxyKeys {
		saved, ok := env.pop(ProxyEnvSavedPrefix+key, environ)
		if ok && !usersDotenvValue(key) {
			env.set(key, saved, environ)
		}
	}
	env.pop(ProxyEnvMarker, environ)
}

// After the update's dotenv load the user's dotenv proxy and NO_PROXY beat the system ones the desktop injected
func PreferDotenvProxyEnv(environ map[string]string) {
	env := newProxyEnv(environ)
	if _, ok := env.get(ProxyEnvMarker); !ok {
		return
	}
	for _, name := range []string{"https_proxy", "http_proxy", "all_proxy"} {
		if len(dotenvPublishedValues(name)) > 0 {
			restoreUserProxyEnv(env, environ)
			return
		}
	}
	published := dotenvPublishedValues("no_proxy")
	var users *string
	for _, key := range env.keys {
		if value := env.vals[key]; strings.ToLower(key) == "no_proxy" && slices.Contains(published, value) {
			users = &value
			break
		}
	}
	if users == nil {
		return
	}
	userEntries := provider.NoProxyEntries(users)
	if slices.Contains(userEntries, "*") {
		restoreUserProxyEnv(env, environ)
		return
	}
	own := map[string]bool{}
	for _, key := range proxyNoProxyKeys {
		saved, _ := env.get(ProxyEnvSavedPrefix + key)
		for _, entry := range provider.NoProxyEntries(&saved) {
			own[entry] = true
		}
	}
	var added []string
	for _, key := range proxyNoProxyKeys {
		value, ok := env.get(key)
		if !ok || value == *users {
			continue
		}
		for _, entry := range provider.NoProxyEntries(&value) {
			if !own[entry] {
				added = append(added, entry)
			}
		}
	}
	var merged []string
	for _, entry := range append(userEntries, added...) {
		if !slices.Contains(merged, entry) {
			merged = append(merged, entry)
		}
	}
	for _, key := range proxyNoProxyKeys {
		if _, ok := env.get(key); ok {
			env.set(key, strings.Join(merged, ","), environ)
		}
	}
}
