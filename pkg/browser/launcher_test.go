package browser

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestHeadedOnlyWhenConfiguredAndADisplayExists(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux display rule")
	}
	env := testEnv(t)
	for _, c := range []struct {
		headed         bool
		display        string
		expectHeadless bool
	}{{false, ":1", true}, {true, ":1", false}, {true, "", true}} {
		procEnv := map[string]string{}
		if c.display != "" {
			procEnv["DISPLAY"] = c.display
		}
		headed := c.headed
		if resolveHeaded(env, &headed, procEnv) != !c.expectHeadless {
			t.Errorf("%+v", c)
		}
	}
}

func TestPackagedOnlyFlagsNeverReachAnExplicitBinary(t *testing.T) {
	env := testEnv(t)
	pin(t, &needsChromiumSandboxBypass, func() bool { return true })
	pin(t, &launcherChromiumArgs, func(*tools.Env) []string { return []string{"--lang=de"} })
	packaged := buildChromiumArgv(env, "/c", "/p", true, false, true, nil)
	dock := buildChromiumArgv(env, "/c", "/p", false, true, true, nil)
	real := buildChromiumArgv(env, "/c", "/p", false, true, false, nil)
	if !slices.Contains(packaged, "--password-store=basic") || slices.Contains(real, "--password-store=basic") {
		t.Fatal("password store flag")
	}
	if !slices.Contains(packaged, "--headless=new") || slices.Contains(real, "--headless=new") {
		t.Fatal("headless flag")
	}
	for _, flag := range chromiumSandboxBypassArgs {
		if !slices.Contains(packaged, flag) || !slices.Contains(dock, flag) || slices.Contains(real, flag) {
			t.Fatalf("sandbox flag %s", flag)
		}
	}
	for _, argv := range [][]string{packaged, dock, real} {
		if !slices.Contains(argv, "--lang=de") || argv[len(argv)-1] != "about:blank" || argv[1] != "--remote-debugging-port=0" || argv[2] != "--user-data-dir=/p" {
			t.Fatalf("argv %v", argv)
		}
	}
	pin(t, &launcherChromiumArgs, func(*tools.Env) []string { return []string{"--no-sandbox"} })
	if argv := buildChromiumArgv(env, "/c", "/p", true, false, true, []string{"--x"}); strings.Count(strings.Join(argv, " "), "--no-sandbox") != 1 || !slices.Contains(argv, "--x") {
		t.Fatalf("configured bypass flag duplicated %v", argv)
	}
}

func TestThrowawayDirNamesCarryOwnerAndNamespace(t *testing.T) {
	pin(t, &launcherStartTime, func(int) (int64, bool) { return 99, true })
	dir := defaultUserDataDir("browser-exec-default", "/h")
	if !strings.HasPrefix(dir, "/h/cache/browser/chromium/") || !throwawayDirRE.MatchString(dir[len("/h/cache/browser/chromium/"):]) {
		t.Fatalf("dir %q", dir)
	}
	pin(t, &launcherStartTime, func(int) (int64, bool) { return 0, false })
	if bare := defaultUserDataDir("browser-exec-default", "/h"); !throwawayDirRE.MatchString(bare[len("/h/cache/browser/chromium/"):]) || strings.Contains(bare, "-99") {
		t.Fatalf("bare %q", bare)
	}
}

func TestDevtoolsFileNeedsBothLines(t *testing.T) {
	dir := t.TempDir()
	write := func(text string) { os.WriteFile(filepath.Join(dir, devtoolsFile), []byte(text), 0o600) }
	if _, _, ok, err := readDevtoolsFile(dir); ok || err != nil {
		t.Fatal("missing file read")
	}
	for text, want := range map[string]int{"\ufeff9222\n/devtools/browser/abc\n": 9222, "  41000 \n/devtools/browser/x": 41000,
		"٤٢\n/devtools/browser/x": 42, "9222\n": -1, "9222\n/json\n": -1, "-1\n/devtools/browser/x": -1, "\n/devtools/browser/x": -1} {
		write(text)
		port, path, ok, err := readDevtoolsFile(dir)
		if err != nil || ok != (want >= 0) || (ok && (port != want || !strings.HasPrefix(path, "/devtools/browser/"))) {
			t.Errorf("%q got %d %q %v %v", text, port, path, ok, err)
		}
	}
	write("²\n/devtools/browser/x")
	if _, _, _, err := readDevtoolsFile(dir); err == nil {
		t.Error("superscript port accepted")
	}
	write("92\xff\n/devtools/browser/x")
	if _, _, _, err := readDevtoolsFile(dir); err == nil {
		t.Error("invalid utf-8 accepted")
	}
}

func TestLogTailKeepsTheLastBytes(t *testing.T) {
	dir := t.TempDir()
	if logTail(dir) != "" {
		t.Fatal("missing log")
	}
	os.WriteFile(filepath.Join(dir, launchLog), []byte(strings.Repeat("x", 2000)+"\xe2\x82 sandbox failed \n"), 0o600)
	tail := logTail(dir)
	if !strings.HasSuffix(tail, "� sandbox failed") || !strings.HasPrefix(tail, "xxx") || len(strings.TrimRight(tail, "� sandbox failed")) > stderrTailChars {
		t.Fatalf("tail %q", tail[len(tail)-30:])
	}
}

func TestSingletonOwnerAndRecordNames(t *testing.T) {
	dir := t.TempDir()
	if _, _, ok := singletonOwner(dir); ok {
		t.Fatal("no lock read")
	}
	os.Symlink("my-host.local-4242", filepath.Join(dir, "SingletonLock"))
	if host, pid, ok := singletonOwner(dir); !ok || host != "my-host.local" || pid != 4242 {
		t.Fatalf("owner %q %d %v", host, pid, ok)
	}
	bad := t.TempDir()
	os.Symlink("host-abc", filepath.Join(bad, "SingletonLock"))
	if _, _, ok := singletonOwner(bad); ok {
		t.Fatal("non numeric pid read")
	}
	records := t.TempDir()
	stemText := profileDigest(dir)[:16] + "-77"
	for _, name := range []string{stemText + ".json", stemText + "-n0123456789ab.json", stemText + "-nzz.json", stemText + "7.json", "other-77.json"} {
		os.WriteFile(filepath.Join(records, name), []byte("{}"), 0o600)
	}
	if got := recordNames(records, dir+"/", int64(77)); len(got) != 2 || got[0] != stemText+"-n0123456789ab.json" || got[1] != stemText+".json" {
		t.Fatalf("names %v", got)
	}
}

func TestDevtoolsEndpointLiveMatchesTheBrowserID(t *testing.T) {
	body := `{"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/browser/abc"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	port, _ := strconv.Atoi(srv.URL[strings.LastIndexByte(srv.URL, ':')+1:])
	if !devtoolsEndpointLive(port, "/devtools/browser/abc") || devtoolsEndpointLive(port, "/devtools/browser/other") {
		t.Fatal("browser id match")
	}
	body = `[1]`
	if devtoolsEndpointLive(port, "/devtools/browser/abc") {
		t.Fatal("non object answered live")
	}
	srv.Close()
	if devtoolsEndpointLive(port, "/devtools/browser/abc") {
		t.Fatal("closed port answered live")
	}
}
