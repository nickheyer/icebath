package profiles

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Routes slog output into a buffer for the duration of the test
func captureLogs(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func servedMap(t *testing.T, multiplex, standalone, parked bool) map[string]string {
	t.Helper()
	serve, err := ProfilesToServe(context.Background(), multiplex, standalone, parked)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, s := range serve {
		out[s.Name] = s.Home
	}
	return out
}

func writeConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProfilesToServeOffReturnsOnlyActiveNamed(t *testing.T) {
	tmp := profileEnv(t)
	ResetStandaloneMemo()
	coder := makeProfile(t, tmp, "coder")
	t.Setenv(home.EnvHome, coder)
	home.ResetKeyCache()
	serve, err := ProfilesToServe(context.Background(), false, false, false)
	if err != nil || len(serve) != 1 || serve[0].Name != "coder" {
		t.Fatalf("%v %v", serve, err)
	}
	if dir, _ := GetProfileDir("coder"); serve[0].Home != dir {
		t.Fatalf("home %q want %q", serve[0].Home, dir)
	}
}

func TestProfilesToServeOnReturnsDefaultPlusAllNamed(t *testing.T) {
	tmp := profileEnv(t)
	ResetStandaloneMemo()
	coder := makeProfile(t, tmp, "coder")
	makeProfile(t, tmp, "writer")
	serve := servedMap(t, true, false, false)
	if len(serve) != 3 || serve["default"] != DefaultHome() || serve["coder"] != coder || serve["writer"] == "" {
		t.Fatalf("%v", serve)
	}
}

func TestStandaloneProfileExcludedUnlessIncluded(t *testing.T) {
	tmp := profileEnv(t)
	ResetStandaloneMemo()
	solo := makeProfile(t, tmp, "solo")
	makeProfile(t, tmp, "member")
	writeConfig(t, solo, "gateway:\n  standalone: true\n")
	if serve := servedMap(t, true, false, false); len(serve) != 2 || serve["member"] == "" || serve["solo"] != "" {
		t.Fatalf("%v", serve)
	}
	if serve := servedMap(t, true, true, false); len(serve) != 3 || serve["solo"] == "" {
		t.Fatalf("%v", serve)
	}
}

func TestParkedProfileSkippedUnlessIncluded(t *testing.T) {
	tmp := profileEnv(t)
	ResetStandaloneMemo()
	parked := makeProfile(t, tmp, "parked")
	if err := os.WriteFile(ParkedMarkerPath(parked), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if serve := servedMap(t, true, false, false); serve["parked"] != "" {
		t.Fatalf("%v", serve)
	}
	if serve := servedMap(t, true, false, true); serve["parked"] != parked {
		t.Fatalf("%v", serve)
	}
}

func TestDefaultProfileWithStandaloneKeyServedWithOneWarning(t *testing.T) {
	profileEnv(t)
	ResetStandaloneMemo()
	logs := captureLogs(t)
	def := DefaultHome()
	writeConfig(t, def, "gateway:\n  standalone: true\n")
	serve := servedMap(t, true, false, false)
	for range 2 {
		if v, err := ProfileIsStandalone(context.Background(), def); v || err != nil {
			t.Fatalf("default standalone %v %v", v, err)
		}
	}
	if len(serve) != 1 || serve["default"] != def {
		t.Fatalf("%v", serve)
	}
	if n := strings.Count(logs.String(), "ignored on the default profile"); n != 1 {
		t.Fatalf("warnings %d", n)
	}
}

func TestStandaloneMalformedConfigDoesNotBreakRoster(t *testing.T) {
	for _, content := range []string{"gateway: [", "[]\n", "null\n", "", "gateway: false\n"} {
		tmp := profileEnv(t)
		ResetStandaloneMemo()
		logs := captureLogs(t)
		solo := makeProfile(t, tmp, "solo")
		writeConfig(t, solo, content)
		for range 2 {
			if v, err := ProfileIsStandalone(context.Background(), solo); v || err != nil {
				t.Fatalf("%q standalone %v %v", content, v, err)
			}
			if servedMap(t, true, false, false)["solo"] == "" {
				t.Fatalf("%q solo not served", content)
			}
		}
		want := 0
		if content == "gateway: [" {
			want = 1
		}
		if n := strings.Count(logs.String(), "Cannot read gateway.standalone"); n != want {
			t.Fatalf("%q warnings %d\n%s", content, n, logs)
		}
	}
}

func TestStandaloneIOFailureIsBoundedAndRecovers(t *testing.T) {
	denied := &fs.PathError{Op: "open", Path: "config.yaml", Err: syscall.EACCES}
	for _, failure := range []string{"stat", "read", "decode"} {
		tmp := profileEnv(t)
		ResetStandaloneMemo()
		logs := captureLogs(t)
		solo := makeProfile(t, tmp, "solo")
		writeConfig(t, solo, "gateway:\n  standalone: true\n")
		oldVersion, oldRead := configCacheVersion, readUserConfigRaw
		switch failure {
		case "stat":
			configCacheVersion = func(context.Context, string) ([]any, error) { return nil, denied }
		case "read":
			readUserConfigRaw = func(context.Context, string) (map[string]any, error) { return nil, denied }
		case "decode":
			readUserConfigRaw = func(context.Context, string) (map[string]any, error) {
				return nil, errors.New("yaml: invalid leading UTF-8 octet")
			}
		}
		for range 2 {
			if v, err := ProfileIsStandalone(context.Background(), solo); v || err != nil {
				t.Fatalf("%s %v %v", failure, v, err)
			}
		}
		configCacheVersion, readUserConfigRaw = oldVersion, oldRead
		if n := strings.Count(logs.String(), "Cannot read gateway.standalone"); n != 1 {
			t.Fatalf("%s warnings %d", failure, n)
		}
		if v, err := ProfileIsStandalone(context.Background(), solo); !v || err != nil {
			t.Fatalf("%s did not recover %v %v", failure, v, err)
		}
	}
}

func TestStandaloneMemoIsPerHomeAndInvalidatesOnReplacement(t *testing.T) {
	tmp := profileEnv(t)
	ResetStandaloneMemo()
	alpha, beta := makeProfile(t, tmp, "alpha"), makeProfile(t, tmp, "beta")
	writeConfig(t, alpha, "gateway:\n  standalone: true\n")
	check := func(dir string, want bool) {
		t.Helper()
		if v, err := ProfileIsStandalone(context.Background(), dir); v != want || err != nil {
			t.Fatalf("%s %v %v", dir, v, err)
		}
	}
	check(alpha, true)
	check(beta, false)
	check(alpha, true)
	replacement := filepath.Join(alpha, "replacement.yaml")
	if err := os.WriteFile(replacement, []byte("gateway:\n  standalone: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, filepath.Join(alpha, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	check(alpha, false)
	check(beta, false)
	check(alpha, false)
}

func TestStandaloneTruthyTokens(t *testing.T) {
	cases := map[any]bool{" Yes ": true, "on": true, "1": true, "maybe": false, "off": false, true: true, 0: false, nil: false}
	for v, want := range cases {
		if standaloneTruthy(v) != want {
			t.Fatalf("%v", v)
		}
	}
}

func TestStandaloneUndecodableConfigWarnsOnceAndRereads(t *testing.T) {
	tmp := profileEnv(t)
	ResetStandaloneMemo()
	logs := captureLogs(t)
	solo := makeProfile(t, tmp, "solo")
	writeConfig(t, solo, "gateway:\n  standalone: \xff\xfe\n")
	for range 2 {
		if v, err := ProfileIsStandalone(context.Background(), solo); v || err != nil {
			t.Fatalf("%v %v", v, err)
		}
	}
	if n := strings.Count(logs.String(), "Cannot read gateway.standalone"); n != 1 {
		t.Fatalf("warnings %d\n%s", n, logs)
	}
	standaloneMu.Lock()
	memo := standaloneMemos[solo]
	standaloneMu.Unlock()
	if memo.result != nil {
		t.Fatalf("decode failure cached as config %v", *memo.result)
	}
}
