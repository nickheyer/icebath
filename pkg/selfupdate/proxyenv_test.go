package selfupdate

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// Applies a dotenv body to the process env with override and publishes its values case insensitively
func peLoadDotenv(t *testing.T, body string) {
	t.Helper()
	published := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		key, value, _ := strings.Cut(line, "=")
		published[key] = value
		t.Setenv(key, value)
	}
	previous := dotenvPublishedValues
	dotenvPublishedValues = func(name string) []string {
		var out []string
		for key, value := range published {
			if strings.EqualFold(key, name) {
				out = append(out, value)
			}
		}
		return out
	}
	t.Cleanup(func() { dotenvPublishedValues = previous })
}

// Unsets every proxy and marker variable for the test
func peClearProxyEnv(t *testing.T) {
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy",
		ProxyEnvMarker, ProxyEnvSavedPrefix + "NO_PROXY", ProxyEnvSavedPrefix + "no_proxy"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

func peLookup(keys ...string) map[string]any {
	out := map[string]any{}
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			out[key] = value
		} else {
			out[key] = nil
		}
	}
	return out
}

func TestMarkedUpdateProxyEnvIsUndoneAndTheUsersOwnEnvKept(t *testing.T) {
	peLoadDotenv(t, "UNRELATED=1")
	user := map[string]string{"PATH": "/usr/bin", "no_proxy": "corp.internal", "ALL_PROXY": "socks5h://user:1080"}
	env := map[string]string{"HTTPS_PROXY": "http://10.0.0.7:7890", "https_proxy": "http://10.0.0.7:7890",
		"NO_PROXY": "corp.internal,localhost,10.0.0.0/8", "no_proxy": "corp.internal,localhost,10.0.0.0/8",
		ProxyEnvMarker: "HTTPS_PROXY,https_proxy,NO_PROXY,PATH", ProxyEnvSavedPrefix + "no_proxy": "corp.internal"}
	for key, value := range user {
		if key != "no_proxy" {
			env[key] = value
		}
	}
	RestoreUserProxyEnv(env)
	if !reflect.DeepEqual(env, user) {
		t.Fatalf("env %v", env)
	}
	unmarked := map[string]string{"HTTPS_PROXY": "http://mine:3128", "NO_PROXY": "lan"}
	RestoreUserProxyEnv(unmarked)
	if !reflect.DeepEqual(unmarked, map[string]string{"HTTPS_PROXY": "http://mine:3128", "NO_PROXY": "lan"}) {
		t.Fatalf("unmarked %v", unmarked)
	}
	if _, ok := os.LookupEnv(ProxyEnvMarker); ok {
		t.Fatal("marker leaked into the process env")
	}
}

func TestAProxyTheUsersDotenvSetsSurvivesTheRestore(t *testing.T) {
	peClearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://10.0.0.7:7890")
	t.Setenv("HTTP_PROXY", "http://10.0.0.7:7890")
	t.Setenv("NO_PROXY", "corp.internal,localhost,10.0.0.0/8")
	t.Setenv(ProxyEnvMarker, "HTTPS_PROXY,https_proxy,HTTP_PROXY,http_proxy")
	t.Setenv(ProxyEnvSavedPrefix+"NO_PROXY", "corp.internal")
	peLoadDotenv(t, "https_proxy=http://user:pw@proxy.corp:3128\nNO_PROXY=lan.example")
	RestoreUserProxyEnv(nil)
	want := map[string]any{"https_proxy": "http://user:pw@proxy.corp:3128", "HTTP_PROXY": nil, "NO_PROXY": "lan.example", ProxyEnvSavedPrefix + "NO_PROXY": nil}
	if got := peLookup("https_proxy", "HTTP_PROXY", "NO_PROXY", ProxyEnvSavedPrefix+"NO_PROXY"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

// The env the desktop hands icebath update with the system proxy in both spellings, then the dotenv load
func peDesktopInjectedEnv(t *testing.T, dotenv string, ownNoProxy *string) {
	t.Helper()
	peClearProxyEnv(t)
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(key, "http://10.0.0.7:7890")
	}
	marked := "HTTPS_PROXY,https_proxy,HTTP_PROXY,http_proxy"
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		parts := []string{"localhost", "pypi.org"}
		if ownNoProxy != nil {
			parts = append([]string{*ownNoProxy}, parts...)
			t.Setenv(ProxyEnvSavedPrefix+key, *ownNoProxy)
		} else {
			marked += "," + key
		}
		t.Setenv(key, strings.Join(parts, ","))
	}
	t.Setenv(ProxyEnvMarker, marked)
	peLoadDotenv(t, dotenv)
}

func TestIcebathUpdateSendsTheUpdateToTheUsersDotenvProxy(t *testing.T) {
	peDesktopInjectedEnv(t, "HTTPS_PROXY=http://proxy.corp:3128\nNO_PROXY=mirror.corp", nil)
	PreferDotenvProxyEnv(nil)
	want := map[string]any{"HTTPS_PROXY": "http://proxy.corp:3128", "https_proxy": nil, "HTTP_PROXY": nil, "http_proxy": nil,
		"NO_PROXY": "mirror.corp", "no_proxy": nil, ProxyEnvMarker: nil}
	if got := peLookup("HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", ProxyEnvMarker); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	own := "old.lan"
	peDesktopInjectedEnv(t, "NO_PROXY=mirror.corp", &own)
	PreferDotenvProxyEnv(nil)
	if os.Getenv("NO_PROXY") != "mirror.corp,localhost,pypi.org" || os.Getenv("no_proxy") != "mirror.corp,localhost,pypi.org" {
		t.Fatalf("NO_PROXY %q no_proxy %q", os.Getenv("NO_PROXY"), os.Getenv("no_proxy"))
	}
}

func TestPreferDotenvProxyEnvWildcardAndUnmarked(t *testing.T) {
	peDesktopInjectedEnv(t, "NO_PROXY=*", nil)
	PreferDotenvProxyEnv(nil)
	if got := peLookup("https_proxy", "NO_PROXY", "no_proxy", ProxyEnvMarker); !reflect.DeepEqual(got, map[string]any{"https_proxy": nil, "NO_PROXY": "*", "no_proxy": nil, ProxyEnvMarker: nil}) {
		t.Fatalf("got %v", got)
	}
	env := map[string]string{"HTTPS_PROXY": "http://10.0.0.7:7890"}
	PreferDotenvProxyEnv(env)
	if env["HTTPS_PROXY"] != "http://10.0.0.7:7890" {
		t.Fatal("unmarked env changed")
	}
}
