package pm

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Drops every ambient uv and pip setting and disables pip config files, the clean index env fixture
func rptCleanIndexEnv(t *testing.T) string {
	t.Helper()
	tmp := isolateHome(t)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "UV_") || strings.HasPrefix(key, "PIP_") {
			t.Setenv(key, "")
			os.Unsetenv(key)
		}
	}
	t.Setenv("PIP_CONFIG_FILE", devNull())
	return tmp
}

// Unsets an environment variable for the rest of the test
func rptUnsetenv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	os.Unsetenv(key)
}

// Pip's index and trusted host bridge into uv while uv's own selection settings never forward
func TestIndexConfigPipIndexReachesUVButAmbientUVSelectionDoesNot(t *testing.T) {
	rptCleanIndexEnv(t)
	t.Setenv("PIP_INDEX_URL", "https://mirror.example/simple")
	t.Setenv("PIP_TRUSTED_HOST", "mirror.example")
	t.Setenv("UV_HTTP_TIMEOUT", "300")
	t.Setenv("UV_INDEX_CORP_PASSWORD", "s3cret")
	t.Setenv("UV_PYTHON", "/poison/python")
	t.Setenv("UV_CACHE_DIR", "/poison/cache")
	t.Setenv("UV_PROJECT_ENVIRONMENT", "/poison/venv")
	settings, err := BridgedIndexSettings(environMap(), "")
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"UV_INDEX_URL": "https://mirror.example/simple", "UV_INSECURE_HOST": "mirror.example",
		"UV_HTTP_TIMEOUT": "300", "UV_INDEX_CORP_PASSWORD": "s3cret",
	} {
		if settings[key] != want {
			t.Fatalf("%s = %q, want %q", key, settings[key], want)
		}
	}
	for _, key := range []string{"UV_PYTHON", "UV_CACHE_DIR", "UV_PROJECT_ENVIRONMENT"} {
		if _, ok := settings[key]; ok {
			t.Fatalf("%s forwarded", key)
		}
	}
}

// Pip conf index reaches uv raw and only while uv has no index of its own
func TestIndexConfigPipConfIsBridgedOnlyWhenUVHasNoIndex(t *testing.T) {
	tmp := rptCleanIndexEnv(t)
	pipConf := filepath.Join(tmp, "pip.conf")
	writeFile(t, pipConf, []byte("[global]\nindex-url = https://user:p%40ss@mirror.example/simple\n"), 0o666)
	t.Setenv("PIP_CONFIG_FILE", pipConf)
	if got, err := BridgedIndexSettings(environMap(), ""); err != nil || got["UV_INDEX_URL"] != "https://user:p%40ss@mirror.example/simple" {
		t.Fatalf("bridged index %q", got)
	}
	t.Setenv("UV_DEFAULT_INDEX", "https://explicit.example/simple")
	settings, err := BridgedIndexSettings(environMap(), "")
	if err != nil {
		t.Fatal(err)
	}
	if settings["UV_DEFAULT_INDEX"] != "https://explicit.example/simple" {
		t.Fatalf("default index %q", settings["UV_DEFAULT_INDEX"])
	}
	if _, ok := settings["UV_INDEX_URL"]; ok {
		t.Fatal("pip conf bridged over an explicit uv index")
	}
}

// Pip conf values read the way RawConfigParser reads them, errors yielding no index
func TestIndexConfigPipConfReadsLikeRawConfigParser(t *testing.T) {
	tmp := rptCleanIndexEnv(t)
	pipConf := filepath.Join(tmp, "pip.conf")
	env := map[string]string{"PIP_CONFIG_FILE": pipConf, "XDG_CONFIG_DIRS": filepath.Join(tmp, "xdg"), "XDG_CONFIG_HOME": filepath.Join(tmp, "config")}
	for text, want := range map[string]string{
		"[global]\nindex-url = https://a.example/simple\n\n  continued\n":          "https://a.example/simple\n\ncontinued",
		"[DEFAULT]\nindex-url = https://d.example\n[DEFAULT]\nx=1\n[global]\n":     "https://d.example",
		"[global]\r\nindex-url = https://cr.example\r  more\r":                     "https://cr.example\nmore",
		"[global]\nindex-url = https://a.example\n\f more\n":                       "https://a.example\nmore",
		"[global]\nindex-url = https://a.example\n\x1f more\n":                     "https://a.example\nmore",
		"[global]\nindex-url = https://a.example \x1f\n":                           "https://a.example",
		"[global] trailing\nIndex-URL: https://colon.example\n":                    "https://colon.example",
		"[global]\nindex-url = https://a.example\n# note\n  kept\n":                "https://a.example\nkept",
		"[global]\nindex-url = https://a.example\n[global]\n":                      "",
		"index-url = https://headless.example\n":                                   "",
		"[global]\nindex-url = https://a.example\nindex-url = https://b.example\n": "",
		"[global]\n= https://nameless.example\n":                                   "",
		"[global]\nno delimiter here\n":                                            "",
		"[other]\nindex-url = https://other.example\n":                             "",
	} {
		writeFile(t, pipConf, []byte(text), 0o666)
		if got, err := PipConfIndexURL(env, ""); err != nil || got != want {
			t.Fatalf("PipConfIndexURL(%q) = %q, want %q", text, got, want)
		}
	}
}

// Tilde expansion follows posixpath and ntpath expanduser
func TestIndexConfigExpandUserMatchesPython(t *testing.T) {
	isolateHome(t)
	if runtime.GOOS != "windows" {
		t.Setenv("HOME", "/tmp/x/")
		for path, want := range map[string]string{"~/a": "/tmp/x/a", "~nosuchuserxyz/a": "~nosuchuserxyz/a", "x~": "x~"} {
			if got := expandUser(path); got != want {
				t.Fatalf("expandUser(%q) = %q, want %q", path, got, want)
			}
		}
		if root, err := user.Lookup("root"); err == nil {
			if got := expandUser("~root/a"); got != strings.TrimRight(root.HomeDir, "/")+"/a" {
				t.Fatalf("expandUser(~root/a) = %q", got)
			}
		}
		t.Setenv("HOME", "")
		if got := expandUser("~/a"); got != "/a" {
			t.Fatalf("empty HOME gave %q", got)
		}
		if got := expandUser("~"); got != "/" {
			t.Fatalf("bare tilde with empty HOME gave %q", got)
		}
	}
	for _, key := range []string{"USERPROFILE", "HOMEPATH", "HOMEDRIVE", "USERNAME"} {
		rptUnsetenv(t, key)
	}
	t.Setenv("USERPROFILE", `C:\Users\nick`)
	t.Setenv("USERNAME", "nick")
	for path, want := range map[string]string{`~\x`: `C:\Users\nick\x`, `~bob\x`: `C:\Users\bob\x`, `~nick/x`: `C:\Users\nick/x`} {
		if got := ntExpandUser(path); got != want {
			t.Fatalf("ntExpandUser(%q) = %q, want %q", path, got, want)
		}
	}
	t.Setenv("USERPROFILE", `C:\Profiles\odd`)
	if got := ntExpandUser(`~bob\x`); got != `~bob\x` {
		t.Fatalf("unguessable user gave %q", got)
	}
	rptUnsetenv(t, "USERPROFILE")
	if got := ntExpandUser(`~\x`); got != `~\x` {
		t.Fatalf("no profile gave %q", got)
	}
	t.Setenv("HOMEDRIVE", "C:")
	t.Setenv("HOMEPATH", `\Users\nick`)
	if got := ntExpandUser(`~\x`); got != `C:\Users\nick\x` {
		t.Fatalf("home drive and path gave %q", got)
	}
	for _, tc := range []struct{ got, want string }{
		{pypath.NtJoin("C:", `\Users\nick`), `C:\Users\nick`},
		{pypath.NtJoin(`\\server\share`, "x"), `\\server\share\x`},
		{pypath.NtJoin("C:", `D:\x`), `D:\x`},
		{pypath.NtJoin(`c:\a`, "C:b"), `C:\a\b`},
		{pypath.NtJoin("X", ""), `X\`},
		{pypath.NtDirname(`\\server\share\nick`), `\\server\share\`},
		{pypath.NtDirname(`C:\Users\nick`), `C:\Users`},
		{pypath.NtDirname(`C:\nick`), `C:\`},
		{pypath.NtBasename("C:foo"), "foo"},
		{pypath.NtDirname(`\\?\UNC\srv\sh\u`), `\\?\UNC\srv\sh\`},
	} {
		if tc.got != tc.want {
			t.Fatalf("ntpath gave %q, want %q", tc.got, tc.want)
		}
	}
}

// A pip config that is not UTF-8 raises Python's decode error instead of being guessed at
func TestPipConfigInvalidUTF8RaisesLikePython(t *testing.T) {
	isolateHome(t)
	conf := filepath.Join(t.TempDir(), "pip.conf")
	writeFile(t, conf, []byte("[global]\nindex-url = x\xff\n"), 0o644)
	_, err := PipConfIndexURL(map[string]string{"PIP_CONFIG_FILE": conf}, "")
	want := "'utf-8' codec can't decode byte 0xff in position 22: invalid start byte"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %s", err, want)
	}
	if _, err := BridgedIndexSettings(map[string]string{"PIP_CONFIG_FILE": conf}, ""); err == nil || err.Error() != want {
		t.Fatalf("bridged got %v", err)
	}
}
