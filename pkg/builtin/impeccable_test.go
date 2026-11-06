package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/commands"
)

func TestImpeccableHermesRenderMatchesGolden(t *testing.T) {
	golden := filepath.Join(refsRoot, "impeccable", ".hermes", "skills", "impeccable")
	rendered, err := renderImpeccableSkill(".icebath/skills/impeccable/scripts")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	err = filepath.WalkDir(golden, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(golden, p)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		want := strings.ReplaceAll(string(data), ".hermes/skills/impeccable/scripts", ".icebath/skills/impeccable/scripts")
		got, ok := rendered[filepath.ToSlash(rel)]
		seen++
		if !ok {
			t.Errorf("missing rendered %s", rel)
		} else if string(got.Data) != want {
			t.Errorf("%s differs from the hermes build", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != len(rendered) {
		t.Fatalf("golden has %d files, rendered %d", seen, len(rendered))
	}
	if rendered["scripts/impeccable"].Mode != 0o755 {
		t.Fatal("launcher lost its executable bit")
	}
}

func TestImpeccableCommandDefShapeAndDispatch(t *testing.T) {
	var gotKey, gotArgs string
	def, err := impeccableCommandDef("impeccable", func(ctx context.Context, key, instruction string) (string, bool, error) {
		gotKey, gotArgs = key, instruction
		return "SKILL TURN " + instruction, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantHint := "[shape · audit|critique · animate|bolder|colorize|delight|layout|overdrive|quieter|typeset · adapt|clarify|distill · harden|onboard|optimize|polish · init|document|extract|live|generate] [target]"
	if def.Name != "impeccable" || len(def.Subcommands) != 24 || def.Subcommands[0] != "craft" || def.ArgsHint != wantHint || def.Plugin != "impeccable" {
		t.Fatalf("def %+v", def)
	}
	host := &turnHost{}
	if err := def.Handler(context.Background(), &commands.Invocation{Name: "impeccable", Args: "audit header", Host: host}); err != nil {
		t.Fatal(err)
	}
	if gotKey != "/impeccable:impeccable" || gotArgs != "audit header" || host.msg != "SKILL TURN audit header" {
		t.Fatalf("dispatch %q %q %q", gotKey, gotArgs, host.msg)
	}
	if err := def.Handler(context.Background(), &commands.Invocation{Name: "impeccable", Host: struct{}{}}); err == nil {
		t.Fatal("surface without a turn submitter accepted the command")
	}
}

type turnHost struct{ msg string }

func (h *turnHost) SubmitUserTurn(ctx context.Context, message string) error {
	h.msg = message
	return nil
}

// Fake engine scripts directory pinning a version whose linux x64 digest is the given sum
func engineFixture(t *testing.T, sum string) (*engineLauncher, string, map[string]string) {
	scripts := t.TempDir()
	os.WriteFile(filepath.Join(scripts, "VERSION"), []byte("9.9.9\n"), 0o644)
	os.WriteFile(filepath.Join(scripts, "engine-checksums.txt"), []byte("# digests\n9.9.9 impeccable-linux-x64 "+sum+"\n"), 0o644)
	cache := t.TempDir()
	env := map[string]string{"HOME": t.TempDir(), "IMPECCABLE_HOME": cache}
	l := newEngineLauncher(scripts)
	l.Getenv = func(k string) string { return env[k] }
	l.GOOS, l.GOARCH = "linux", "amd64"
	l.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
	l.Stderr = nil
	return l, cache, env
}

const fakeEngine = "#!/bin/sh\necho impeccable-engine 9.9.9\n"

func sumOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestEngineDownloadVerifiedAgainstChecksums(t *testing.T) {
	var hits atomic.Int32
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotPath.Store(r.URL.Path)
		w.Write([]byte(fakeEngine))
	}))
	defer srv.Close()
	l, cache, env := engineFixture(t, sumOf(fakeEngine))
	env["IMPECCABLE_DOWNLOAD_BASE"] = srv.URL
	bin, err := l.resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bin != filepath.Join(cache, "bin", "9.9.9", "impeccable") || gotPath.Load() != "/engine-v9.9.9/impeccable-linux-x64" || !isExecutable(bin) {
		t.Fatalf("bin %s path %v", bin, gotPath.Load())
	}
	if stamp, size := readStamp(bin + ".verified"); stamp != sumOf(fakeEngine) || size != fileSize(bin) {
		t.Fatalf("stamp %s %s", stamp, size)
	}
	if again, err := l.resolve(context.Background()); err != nil || again != bin || hits.Load() != 1 {
		t.Fatalf("cached reuse %s %v hits %d", again, err, hits.Load())
	}
}

func TestEngineRefusesUnverifiedDownloads(t *testing.T) {
	body := fakeEngine
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	cases := []struct {
		name, sum, body string
		status          int
		want            string
	}{
		{"mismatch", sumOf("other"), fakeEngine, 200, "impeccable: checksum mismatch downloading " + srv.URL + "/engine-v9.9.9/impeccable-linux-x64"},
		{"empty", sumOf(""), "", 200, "downloaded file is empty: " + srv.URL + "/engine-v9.9.9/impeccable-linux-x64; refusing the unverified download"},
		{"missing", sumOf(fakeEngine), "", 404, "impeccable: could not download engine 9.9.9 from " + srv.URL},
	}
	for _, c := range cases {
		body, status = c.body, c.status
		l, cache, env := engineFixture(t, c.sum)
		env["IMPECCABLE_DOWNLOAD_BASE"] = srv.URL
		_, err := l.resolve(context.Background())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
		if isFile(filepath.Join(cache, "bin", "9.9.9", "impeccable")) {
			t.Errorf("%s: unverified engine cached", c.name)
		}
	}
	l, _, env := engineFixture(t, sumOf(fakeEngine))
	os.WriteFile(filepath.Join(l.ScriptsDir, "engine-checksums.txt"), []byte("9.9.9 impeccable-darwin-arm64 abc\n"), 0o644)
	env["IMPECCABLE_DOWNLOAD_BASE"] = srv.URL
	if _, err := l.resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "no checksum for engine 9.9.9 (impeccable-linux-x64)") {
		t.Fatalf("no checksum: %v", err)
	}
}

func TestEngineLocalSourcesAndPlatform(t *testing.T) {
	l, _, env := engineFixture(t, sumOf(fakeEngine))
	pre := filepath.Join(t.TempDir(), "engine")
	os.WriteFile(pre, []byte(fakeEngine), 0o755)
	env["IMPECCABLE_BIN"] = pre
	if bin, err := l.resolve(context.Background()); err != nil || bin != pre {
		t.Fatalf("IMPECCABLE_BIN %s %v", bin, err)
	}
	delete(env, "IMPECCABLE_BIN")
	bundled := filepath.Join(l.ScriptsDir, "bin", "linux-x64", "impeccable")
	os.MkdirAll(filepath.Dir(bundled), 0o755)
	os.WriteFile(bundled, []byte(fakeEngine), 0o644)
	if bin, err := l.resolve(context.Background()); err != nil || bin != bundled {
		t.Fatalf("bundled %s %v", bin, err)
	}
	os.Remove(bundled)
	l.GOARCH = "riscv64"
	if _, err := l.resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "impeccable: no engine binary for linux-unknown found") {
		t.Fatalf("unknown arch %v", err)
	}
	if got := strings.Join(l.exports(), " "); got != "IMPECCABLE_SKILL_DIR="+filepath.Dir(l.ScriptsDir)+" IMPECCABLE_SELF="+filepath.Join(l.ScriptsDir, "impeccable") {
		t.Fatalf("exports %s", got)
	}
}

func TestImpeccableDispatchHonoursRoutingAliases(t *testing.T) {
	var got string
	def, err := impeccableCommandDef("impeccable", func(ctx context.Context, key, instruction string) (string, bool, error) {
		got = instruction
		return instruction, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	src, _ := payloadFile(impeccableSkillRoot + "/SKILL.src.md")
	if !strings.Contains(string(src), "- `teach` aliases `init`.") {
		t.Fatal("SKILL.src.md no longer names the teach alias")
	}
	for args, want := range map[string]string{"teach": "init", "teach the checkout flow": "init the checkout flow", "  teach\tpricing": "init\tpricing",
		"teacher page": "teacher page", "audit teach": "audit teach", "craft hero": "craft hero", "": ""} {
		if err := def.Handler(context.Background(), &commands.Invocation{Name: "impeccable", Args: args, Host: &turnHost{}}); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%q dispatched as %q, want %q", args, got, want)
		}
	}
}
