package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyzip"
)

// Skips a case the hermes suite marks POSIX only
func posixOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions only")
	}
}

type zmember struct {
	name string
	data string
}

// Archive written with the Python zipfile writer semantics, one writestr per member
func writeZip(t *testing.T, path string, compression int, members ...zmember) {
	t.Helper()
	zf, err := pyzip.Create(path, compression, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		f, err := zf.Open(m.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(m.data)); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
}

// Archive of files on disk under their arcnames, zipfile.ZipFile.write per member
func writeZipFiles(t *testing.T, path string, files ...[2]string) {
	t.Helper()
	zf, err := pyzip.Create(path, pyzip.Stored, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := zf.Write(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zf.Close(); err != nil {
		t.Fatal(err)
	}
}

// Service calls one revive recorded
type serviceCalls struct{ contexts []string }

// Keeps every import off the host service manager like the autouse fixture, recording ensure calls
func noRealGatewayService(t *testing.T, running bool, runErr error) *serviceCalls {
	t.Helper()
	calls := &serviceCalls{}
	oldEnsure, oldRunning := ensureGatewayService, isServiceRunning
	ensureGatewayService = func(_ context.Context, _ io.Writer, o gateway.ServiceSetupOptions) (bool, error) {
		calls.contexts = append(calls.contexts, o.Context)
		return false, nil
	}
	isServiceRunning = func(context.Context) (bool, error) { return running, runErr }
	t.Cleanup(func() { ensureGatewayService, isServiceRunning = oldEnsure, oldRunning })
	return calls
}

// Points ICEBATH_HOME and pathlib Path.home at test directories
func useHomes(t *testing.T, h, userHome string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", h)
	if userHome != "" {
		t.Cleanup(home.PatchUserHome(userHome))
	}
}

// Runs the import command of the process home with zipfile and force
func runImport(t *testing.T, zipPath string, force bool) (int, string) {
	t.Helper()
	var out bytes.Buffer
	ctx := context.Background()
	code, err := RunImport(ctx, home.Dir(ctx), map[string]any{"zipfile": zipPath, "force": force}, &out)
	if err != nil {
		t.Fatalf("RunImport: %v\n%s", err, out.String())
	}
	return code, out.String()
}

func readText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func writeText(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Damages one member's data in place leaving is_zipfile and the name list intact
func corruptMember(t *testing.T, zipPath, name, damage string) {
	t.Helper()
	zr, err := pyzip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := zr.GetInfo(name)
	zr.Close()
	if err != nil {
		t.Fatal(err)
	}
	dataOffset := int(info.HeaderOffset) + 30 + len(info.Filename) + len(info.Extra)
	raw, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	ho := int(info.HeaderOffset)
	switch damage {
	case "truncated":
		local := raw[ho+4 : ho+26]
		entry := bytes.Index(raw[ho+26:], local) + ho + 26 - 6
		binary.LittleEndian.PutUint32(raw[entry+20:], uint32(info.CompressSize/2))
	case "deflate":
		raw[dataOffset] |= 0b110
	case "bzip2":
		raw[dataOffset+10] ^= 0xFF
	default:
		raw[dataOffset] ^= 0xFF
	}
	if err := os.WriteFile(zipPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidateBackupZipStateDbPasses(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"state.db", "dummy"}, zmember{"sessions/abc.json", "dummy"})
	zr, err := pyzip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if ok, reason := validateBackupZip(zr); !ok {
		t.Fatal(reason)
	}
}

func TestValidationValidateWithConfig(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"config.yaml", "test"})
	zr, err := pyzip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if ok, _ := validateBackupZip(zr); !ok {
		t.Fatal("config.yaml archive rejected")
	}
}

func TestValidationDetectPrefixOnlyDirs(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{".icebath/", ""}, zmember{".icebath/skills/", ""})
	zr, err := pyzip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if got := detectPrefix(zr); got != "" {
		t.Fatalf("prefix %q", got)
	}
}

func TestImportAutoInstallsGatewayService(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	useHomes(t, h, tmp)
	calls := noRealGatewayService(t, false, nil)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"config.yaml", "model: test\n"})
	runImport(t, zipPath, true)
	if len(calls.contexts) == 0 || calls.contexts[0] != "import" {
		t.Fatalf("ensure calls %v", calls.contexts)
	}
}

func TestImportSkipsServiceWhenAlreadyRunning(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	useHomes(t, h, tmp)
	calls := noRealGatewayService(t, true, nil)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"config.yaml", "model: test\n"})
	runImport(t, zipPath, true)
	if len(calls.contexts) != 0 {
		t.Fatalf("ensure called %v", calls.contexts)
	}
}

func TestImportSurvivesServiceLayerImportFailure(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, errors.New("service layer unavailable"))
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"config.yaml", "model: test\n"})
	runImport(t, zipPath, true)
	if got := readText(t, filepath.Join(h, "config.yaml")); got != "model: test\n" {
		t.Fatalf("config %q", got)
	}
}

// Home with a live config the corrupt archive tests must leave untouched
func homeForCorruptImport(t *testing.T) (string, string) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	writeText(t, filepath.Join(h, "config.yaml"), "model: live\n")
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	return tmp, h
}

func TestImportImportRefusesDamagedArchiveBeforeTouchingHome(t *testing.T) {
	for _, tc := range []struct{ damage, compression string }{
		{"deflate", "deflated"}, {"stored", "stored"}, {"bzip2", "bzip2"}, {"truncated", "deflated"},
	} {
		t.Run(tc.damage, func(t *testing.T) {
			tmp, h := homeForCorruptImport(t)
			zipPath := filepath.Join(tmp, "backup.zip")
			var names []string
			for i := range 12 {
				names = append(names, "skills/s"+strconv.Itoa(i)+"/SKILL.md")
			}
			if tc.compression == "bzip2" {
				raw, err := os.ReadFile(filepath.Join("testdata", "bzip2_backup.zip"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(zipPath, raw, 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				method := pyzip.Stored
				if tc.compression == "deflated" {
					method = pyzip.Deflated
				}
				members := []zmember{{"config.yaml", "model: restored\n"}}
				for _, n := range names {
					members = append(members, zmember{n, strings.Repeat("# "+n+"\n", 20)})
				}
				members = append(members, zmember{"sessions/after.json", `{"restored": true}`})
				writeZip(t, zipPath, method, members...)
			}
			for _, n := range names {
				corruptMember(t, zipPath, n, tc.damage)
			}
			if !pyzip.IsZipfile(zipPath) {
				t.Fatal("damaged archive no longer a zip file")
			}
			code, out := runImport(t, zipPath, true)
			if code != 1 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			if !strings.Contains(out, "backup archive is damaged (12 member(s)") {
				t.Fatal(out)
			}
			listed := 0
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "skills/s") {
					listed++
				}
			}
			if listed != 10 || !strings.Contains(out, "... and 2 more") {
				t.Fatalf("listed %d\n%s", listed, out)
			}
			if strings.Contains(out, "Importing") || strings.Contains(strings.ReplaceAll(out, "nothing was restored", ""), "restored") {
				t.Fatal(out)
			}
			if got := readText(t, filepath.Join(h, "config.yaml")); got != "model: live\n" {
				t.Fatalf("config %q", got)
			}
			entries, _ := os.ReadDir(h)
			if len(entries) != 1 || entries[0].Name() != "config.yaml" {
				t.Fatalf("home entries %v", entries)
			}
			if code, _ := runImport(t, zipPath, true); code != 1 {
				t.Fatalf("second import exit %d", code)
			}
		})
	}
}

func TestImportImportIgnoresDamagedPmRuntimeButKeepsPortableData(t *testing.T) {
	tmp, h := homeForCorruptImport(t)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Deflated,
		zmember{"config.yaml", "model: restored\n"},
		zmember{"profiles/coder/installs/python.zip", strings.Repeat("machine-specific\n", 20)},
		zmember{"skills/demo/SKILL.md", "# portable\n"})
	corruptMember(t, zipPath, "profiles/coder/installs/python.zip", "deflate")
	if code, out := runImport(t, zipPath, true); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if readText(t, filepath.Join(h, "config.yaml")) != "model: restored\n" || readText(t, filepath.Join(h, "skills/demo/SKILL.md")) != "# portable\n" {
		t.Fatal("portable data not restored")
	}
	if exists(filepath.Join(h, "profiles/coder/installs")) {
		t.Fatal("PM runtime restored")
	}
}

func TestImportImportSkipsMemberThatRotsAfterPreflightAndReportsIncomplete(t *testing.T) {
	tmp, h := homeForCorruptImport(t)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Deflated,
		zmember{"config.yaml", "model: restored\n"},
		zmember{"damaged.json", `{"will": "fail"}`},
		zmember{"gateway.pid", strings.Repeat("12345\n", 20)},
		zmember{"sessions/after.json", `{"restored": true}`})
	corruptMember(t, zipPath, "gateway.pid", "deflate")
	real := extractMember
	extractMember = func(zr *pyzip.ZipReader, member, target string, mode uint32, ok bool) error {
		if member == "damaged.json" {
			return &pyzip.EOFError{Msg: "Compressed file ended before the end-of-stream marker was reached"}
		}
		return real(zr, member, target, mode, ok)
	}
	t.Cleanup(func() { extractMember = real })
	code, out := runImport(t, zipPath, true)
	if code != 1 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if strings.Contains(out, "damaged (") {
		t.Fatal(out)
	}
	if readText(t, filepath.Join(h, "config.yaml")) != "model: restored\n" {
		t.Fatal("config not restored")
	}
	if exists(filepath.Join(h, "damaged.json")) || exists(filepath.Join(h, "gateway.pid")) {
		t.Fatal("damaged member or runtime file landed")
	}
	if partials, _ := filepath.Glob(filepath.Join(h, ".damaged.json.*.partial")); len(partials) != 0 {
		t.Fatalf("partials %v", partials)
	}
	if readText(t, filepath.Join(h, "sessions", "after.json")) != `{"restored": true}` {
		t.Fatal("later member not restored")
	}
	if !strings.Contains(out, "Warnings (1 files skipped):") || !strings.Contains(out, "Import incomplete") || strings.Contains(out, "has been restored") {
		t.Fatal(out)
	}
}

func TestImportSkippedMemberReportsIncompleteAndExits1(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	locked := filepath.Join(h, "skills", "demo")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored,
		zmember{"config.yaml", "model: test\n"}, zmember{"gateway.pid", "123\n"}, zmember{"skills/demo/SKILL.md", "# demo\n"})
	os.Chmod(locked, 0o555)
	func() {
		defer os.Chmod(locked, 0o755)
		if probe, err := os.CreateTemp(locked, "access"); err == nil {
			probe.Close()
			os.Remove(probe.Name())
			t.Skip("root or Windows: chmod 0555 does not make the directory read-only")
		}
		code, out := runImport(t, zipPath, true)
		if code != 1 || !strings.Contains(out, "Import incomplete") || strings.Contains(out, "Import complete") ||
			!strings.Contains(out, "skills/demo/SKILL.md") || strings.Contains(out, "has been restored") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if code, _ := runImport(t, zipPath, true); code != 1 {
			t.Fatalf("command exit %d", code)
		}
	}()
	code, out := runImport(t, zipPath, true)
	if code != 0 || !strings.Contains(out, "Preserved 1 runtime state file(s)") || !strings.Contains(out, "Done. Your Icebath configuration has been restored.") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestImportPreservesPerProfileGatewayState(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	live := `{"gateway_state": "running"}`
	writeText(t, filepath.Join(h, "profiles", "coder", "gateway_state.json"), live)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored,
		zmember{"config.yaml", "model: test\n"},
		zmember{"profiles/coder/config.yaml", "model: anthropic\n"},
		zmember{"profiles/coder/gateway_state.json", `{"gateway_state": "stopped"}`})
	runImport(t, zipPath, true)
	if readText(t, filepath.Join(h, "profiles", "coder", "config.yaml")) != "model: anthropic\n" {
		t.Fatal("profile config not restored")
	}
	if readText(t, filepath.Join(h, "profiles", "coder", "gateway_state.json")) != live {
		t.Fatal("live gateway state replaced")
	}
}

func TestImportPreservesRuntimePidAndProcessFiles(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	writeText(t, filepath.Join(h, "gateway.pid"), "4242")
	writeText(t, filepath.Join(h, "processes.json"), `{"live": true}`)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored,
		zmember{"config.yaml", "model: test\n"}, zmember{"gateway.pid", "9999"}, zmember{"cron.pid", "8888"},
		zmember{"gateway.lock", "7777"}, zmember{"processes.json", `{"stale": true}`})
	runImport(t, zipPath, true)
	if readText(t, filepath.Join(h, "gateway.pid")) != "4242" || readText(t, filepath.Join(h, "processes.json")) != `{"live": true}` {
		t.Fatal("live runtime files replaced")
	}
	if exists(filepath.Join(h, "cron.pid")) {
		t.Fatal("foreign cron.pid landed")
	}
	var lock map[string]any
	if err := json.Unmarshal([]byte(readText(t, filepath.Join(h, "gateway.lock"))), &lock); err != nil {
		t.Fatal(err)
	}
	canonical, err := gateway.CanonicalHome(h)
	if err != nil {
		t.Fatal(err)
	}
	if lock["hermes_home"] != canonical {
		t.Fatalf("lock %v, want home %s", lock, canonical)
	}
}

func TestImportRestoresSecretFilesWith0600Perms(t *testing.T) {
	posixOnly(t)
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored,
		zmember{"config.yaml", "model: openrouter\n"}, zmember{".env", "OPENROUTER_API_KEY=sk-secret\n"},
		zmember{"auth.json", `{"providers": {"nous": "token"}}`}, zmember{"state.db", "SQLite format 3\x00"},
		zmember{"profiles/coder/.env", "ANTHROPIC_API_KEY=sk-ant-secret\n"})
	runImport(t, zipPath, true)
	for _, rel := range []string{".env", "auth.json", "state.db", "profiles/coder/.env"} {
		st, err := os.Stat(filepath.Join(h, rel))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s restored with mode %o, expected 0o600", rel, st.Mode().Perm())
		}
	}
}

func TestImportEdgeCasesEofDuringConfirmation(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	writeText(t, filepath.Join(h, "config.yaml"), "existing\n")
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"config.yaml", "new\n"})
	old := importInput
	importInput = func(io.Writer, string) (string, error) { return "", cliui.ErrEOF }
	t.Cleanup(func() { importInput = old })
	if code, out := runImport(t, zipPath, false); code != 1 || !strings.Contains(out, "\nAborted.") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// Stream of a member that dies on its first read like a full disk
type explodingMember struct{}

func (explodingMember) Read([]byte) (int, error) {
	return 0, &os.PathError{Op: "read", Path: "", Err: syscall.ENOSPC}
}
func (explodingMember) Close() error { return nil }

// Serves the first open of one member for real and hands every later open a dying stream
func breakMember(t *testing.T, failing string) {
	real := openMember
	seen := 0
	openMember = func(zr *pyzip.ZipReader, member string) (io.ReadCloser, error) {
		if member == failing {
			seen++
			if seen > 1 {
				return explodingMember{}, nil
			}
		}
		return real(zr, member)
	}
	t.Cleanup(func() { openMember = real })
}

func TestImportAtomicWritesFailedMemberLeavesExistingFileIntact(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	original := "model: original\napi_key: keep-me\n"
	writeText(t, filepath.Join(h, "config.yaml"), original)
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"config.yaml", "model: replacement\n"}, zmember{"state.db", ""})
	breakMember(t, "config.yaml")
	runImport(t, zipPath, true)
	if readText(t, filepath.Join(h, "config.yaml")) != original {
		t.Fatal("existing file truncated")
	}
	if left, _ := filepath.Glob(filepath.Join(h, ".config.yaml.*")); len(left) != 0 {
		t.Fatalf("residue %v", left)
	}
}

func TestImportAtomicWritesFailedExternalMemberLeavesExistingFileIntact(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "dst")
	h := filepath.Join(dst, ".icebath")
	honcho := filepath.Join(dst, ".honcho")
	os.MkdirAll(h, 0o755)
	original := `{"peer":"original"}`
	writeText(t, filepath.Join(honcho, "config.json"), original)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"config.yaml", "model: {}\n"}, zmember{"_external/.honcho/config.json", `{"peer":"replacement"}`})
	useHomes(t, h, dst)
	noRealGatewayService(t, false, nil)
	breakMember(t, "_external/.honcho/config.json")
	runImport(t, zipPath, true)
	if readText(t, filepath.Join(honcho, "config.json")) != original {
		t.Fatal("external file truncated")
	}
	if left, _ := filepath.Glob(filepath.Join(honcho, ".config.json.*")); len(left) != 0 {
		t.Fatalf("residue %v", left)
	}
}

func TestImportAtomicWritesSymlinkedTargetKeepsItsSymlink(t *testing.T) {
	posixOnly(t)
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	store := filepath.Join(h, "store")
	os.MkdirAll(store, 0o755)
	real := filepath.Join(store, "config.yaml")
	writeText(t, real, "model: original\n")
	link := filepath.Join(h, "config.yaml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"config.yaml", "model: restored\n"}, zmember{"state.db", ""})
	runImport(t, zipPath, true)
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("import replaced the symlink with a regular file")
	}
	if readText(t, real) != "model: restored\n" {
		t.Fatal("link target not restored")
	}
}

func TestImportAtomicWritesSymlinkedExternalTargetKeepsItsSymlink(t *testing.T) {
	posixOnly(t)
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "dst")
	h := filepath.Join(dst, ".icebath")
	os.MkdirAll(h, 0o755)
	real := filepath.Join(dst, "dotfiles", "honcho.json")
	writeText(t, real, `{"peer":"original"}`)
	link := filepath.Join(dst, ".honcho", "config.json")
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	useHomes(t, h, dst)
	noRealGatewayService(t, false, nil)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"config.yaml", "model: {}\n"}, zmember{"_external/.honcho/config.json", `{"peer":"restored"}`})
	runImport(t, zipPath, true)
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("import replaced the symlink with a regular file")
	}
	if readText(t, real) != `{"peer":"restored"}` {
		t.Fatal("link target not restored")
	}
}

func TestImportAtomicWritesRestorePreservesExistingFileMode(t *testing.T) {
	posixOnly(t)
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	target := filepath.Join(h, "config.yaml")
	writeText(t, target, "model: original\n")
	os.Chmod(target, 0o644)
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"config.yaml", "model: restored\n"}, zmember{"state.db", ""})
	runImport(t, zipPath, true)
	st, _ := os.Stat(target)
	if readText(t, target) != "model: restored\n" || st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}

func TestImportAtomicWritesRestorePreservesExistingFileOwner(t *testing.T) {
	posixOnly(t)
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	target := filepath.Join(h, "config.yaml")
	writeText(t, target, "model: original\n")
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"config.yaml", "model: restored\n"}, zmember{"state.db", ""})
	oldPreserve, oldRestore := preserveFileOwner, restoreFileOwner
	preserveFileOwner = func(p string) *pyfs.FileOwner {
		if exists(p) {
			return &pyfs.FileOwner{UID: 123, GID: 456}
		}
		return nil
	}
	type chown struct {
		path     string
		uid, gid int
	}
	var calls []chown
	restoreFileOwner = func(p string, owner *pyfs.FileOwner) {
		if owner != nil {
			calls = append(calls, chown{p, owner.UID, owner.GID})
		}
	}
	t.Cleanup(func() { preserveFileOwner, restoreFileOwner = oldPreserve, oldRestore })
	runImport(t, zipPath, true)
	if readText(t, target) != "model: restored\n" {
		t.Fatal("not restored")
	}
	if len(calls) != 1 || calls[0] != (chown{target, 123, 456}) {
		t.Fatalf("chown calls %v", calls)
	}
}

// Wraps the atomic publish to record the staged mode of one target name
func spyStagedModes(t *testing.T, name string) *[]uint32 {
	var modes []uint32
	real := atomicReplace
	atomicReplace = func(tmp, dst string) (string, error) {
		if filepath.Base(dst) == name {
			mode, _ := pyfs.PreserveFileMode(tmp)
			modes = append(modes, mode)
		}
		return real(tmp, dst)
	}
	t.Cleanup(func() { atomicReplace = real })
	return &modes
}

func TestImportAtomicWritesModeIsAppliedBeforeTheReplace(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	target := filepath.Join(h, "config.yaml")
	writeText(t, target, "model: original\n")
	os.Chmod(target, 0o644)
	st, _ := os.Stat(target)
	expected := uint32(st.Mode().Perm())
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"config.yaml", "model: restored\n"}, zmember{"state.db", ""})
	staged := spyStagedModes(t, "config.yaml")
	runImport(t, zipPath, true)
	if len(*staged) != 1 || (*staged)[0]&0o777 != expected {
		t.Fatalf("staged %o", *staged)
	}
	st, _ = os.Stat(target)
	if uint32(st.Mode().Perm()) != expected {
		t.Fatalf("published %o", st.Mode().Perm())
	}
}

func TestImportAtomicWritesRestoreDoesNotCarrySetuidOntoArchiveContent(t *testing.T) {
	posixOnly(t)
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	target := filepath.Join(h, "helper.sh")
	writeText(t, target, "#!/bin/sh\necho original\n")
	pyfs.RestoreFileMode(target, 0o6755, true)
	if mode, _ := pyfs.PreserveFileMode(target); mode != 0o6755 {
		t.Skip("filesystem refuses setuid/setgid on a user-owned file")
	}
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"helper.sh", "#!/bin/sh\necho attacker\n"}, zmember{"state.db", ""})
	staged := spyStagedModes(t, "helper.sh")
	runImport(t, zipPath, true)
	published, _ := pyfs.PreserveFileMode(target)
	if readText(t, target) != "#!/bin/sh\necho attacker\n" {
		t.Fatal("not restored")
	}
	if published&0o4000 != 0 || published&0o2000 != 0 {
		t.Fatalf("archive content kept the target's setuid or setgid bit (mode 0o%o)", published)
	}
	if published != 0o755 {
		t.Fatalf("published %o", published)
	}
	if len(*staged) != 1 || (*staged)[0] != 0o755 {
		t.Fatalf("the staged temp file was elevated before publish: %o", *staged)
	}
}

func TestProfileRestorationImportSkipsProfileDirsWithoutConfig(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	wrapperDir := filepath.Join(tmp, ".local", "bin")
	os.MkdirAll(wrapperDir, 0o755)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored,
		zmember{"config.yaml", "model: test\n"}, zmember{"profiles/valid/config.yaml", "model: test\n"},
		zmember{"profiles/empty/readme.txt", "nothing here\n"})
	runImport(t, zipPath, true)
	if !exists(filepath.Join(wrapperDir, "valid")) || exists(filepath.Join(wrapperDir, "empty")) {
		t.Fatal("wrapper set wrong")
	}
}

func TestMemoryProviderExternalPathsImportRestoresExternalToHomeRelativeLocation(t *testing.T) {
	tmp := t.TempDir()
	dst := filepath.Join(tmp, "dst")
	h := filepath.Join(dst, ".icebath")
	os.MkdirAll(h, 0o755)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZip(t, zipPath, pyzip.Stored, zmember{"config.yaml", "model: {}\n"}, zmember{".env", "X=1\n"},
		zmember{"state.db", ""}, zmember{"_external/.honcho/config.json", `{"peer":"bob"}`})
	useHomes(t, h, dst)
	noRealGatewayService(t, false, nil)
	runImport(t, zipPath, true)
	restored := filepath.Join(dst, ".honcho", "config.json")
	if readText(t, restored) != `{"peer":"bob"}` {
		t.Fatal("external file not restored")
	}
	if st, _ := os.Stat(restored); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if exists(filepath.Join(h, "_external")) {
		t.Fatal("external state leaked into the home")
	}
}

// Backup archive of a source home holding config.yaml and .env
func overrideBackupZip(t *testing.T, tmp string) string {
	src := filepath.Join(tmp, "src-home")
	writeText(t, filepath.Join(src, "config.yaml"), "model:\n  provider: anthropic\n")
	writeText(t, filepath.Join(src, ".env"), "ANTHROPIC_API_KEY=sk-test\n")
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZipFiles(t, zipPath, [2]string{filepath.Join(src, "config.yaml"), "config.yaml"}, [2]string{filepath.Join(src, ".env"), ".env"})
	return zipPath
}

func TestImportHonorsHermesHomeOverrideImportTargetsNamedProfileHome(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "icebath-root")
	profile := filepath.Join(root, "profiles", "coder")
	os.MkdirAll(profile, 0o755)
	writeText(t, filepath.Join(root, "config.yaml"), "model:\n  provider: openai\n")
	t.Setenv("ICEBATH_HOME", profile)
	noRealGatewayService(t, false, nil)
	if home.Dir(context.Background()) != profile {
		t.Fatal("process home is not the profile")
	}
	zipPath := overrideBackupZip(t, tmp)
	runImport(t, zipPath, true)
	if readText(t, filepath.Join(profile, "config.yaml")) != "model:\n  provider: anthropic\n" {
		t.Fatal("profile not restored")
	}
	if readText(t, filepath.Join(root, "config.yaml")) != "model:\n  provider: openai\n" {
		t.Fatal("root config clobbered")
	}
}

// Points the platform default home the revive compares against at a test directory
func useNativeDefault(t *testing.T, native string) {
	old := platformDefaultHome
	platformDefaultHome = func() string { return native }
	t.Cleanup(func() { platformDefaultHome = old })
}

func TestImportHonorsHermesHomeOverrideImportSkipsGatewayInstallForNonDefaultHome(t *testing.T) {
	tmp := t.TempDir()
	native := filepath.Join(tmp, "native-default")
	sandbox := filepath.Join(tmp, "sandbox-home")
	os.MkdirAll(sandbox, 0o755)
	writeText(t, filepath.Join(native, "config.yaml"), "model:\n  provider: openai\n")
	t.Setenv("ICEBATH_HOME", sandbox)
	useNativeDefault(t, native)
	calls := noRealGatewayService(t, false, nil)
	runImport(t, overrideBackupZip(t, tmp), true)
	if len(calls.contexts) != 0 {
		t.Fatal("gateway must not be auto-installed for a sandbox restore")
	}
}

func TestImportHonorsHermesHomeOverrideImportInstallsGatewayWhenDefaultHomeIsTarget(t *testing.T) {
	tmp := t.TempDir()
	native := filepath.Join(tmp, "native-default")
	os.MkdirAll(native, 0o755)
	t.Setenv("ICEBATH_HOME", native)
	useNativeDefault(t, native)
	calls := noRealGatewayService(t, false, nil)
	runImport(t, overrideBackupZip(t, tmp), true)
	if len(calls.contexts) == 0 || calls.contexts[0] != "import" {
		t.Fatalf("ensure calls %v", calls.contexts)
	}
}

// Minimal session store with the given sessions and messages per session
func writeSessionDB(t *testing.T, path string, sessions, perSession int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"CREATE TABLE IF NOT EXISTS sessions (session_id TEXT PRIMARY KEY, message_count INTEGER)",
		"CREATE TABLE IF NOT EXISTS messages (id INTEGER PRIMARY KEY, session_id TEXT, content TEXT)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for s := range sessions {
		sid := "sess-" + strconv.Itoa(s)
		if _, err := db.Exec("INSERT INTO sessions VALUES (?, ?)", sid, perSession); err != nil {
			t.Fatal(err)
		}
		for m := range perSession {
			if _, err := db.Exec("INSERT INTO messages (session_id, content) VALUES (?, ?)", sid, sid+"-msg-"+strconv.Itoa(m)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Sessions and messages counted through a fresh connection
func countRows(t *testing.T, path string) [2]int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out [2]int
	if err := db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&out[0]); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&out[1]); err != nil {
		t.Fatal(err)
	}
	return out
}

// Home with a live three session store and an archive of a two session one
func prepareLiveSessionImport(t *testing.T) (string, string, string) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	live := filepath.Join(h, "state.db")
	writeSessionDB(t, live, 3, 4)
	staged := filepath.Join(tmp, "backup-state.db")
	writeSessionDB(t, staged, 2, 2)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZipFiles(t, zipPath, [2]string{staged, "state.db"})
	return h, live, zipPath
}

func TestImportLiveSessionDatabaseLiveHolderSeesImportedRows(t *testing.T) {
	_, live, zipPath := prepareLiveSessionImport(t)
	holder, err := sql.Open("sqlite", live)
	if err != nil {
		t.Fatal(err)
	}
	holder.SetMaxOpenConns(1)
	var n int
	if err := holder.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil || n != 12 {
		t.Fatalf("before %d %v", n, err)
	}
	before, _ := os.Stat(live)
	runImport(t, zipPath, true)
	if err := holder.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil || n != 4 {
		t.Fatalf("holder sees %d %v", n, err)
	}
	holder.Close()
	after, _ := os.Stat(live)
	if !os.SameFile(before, after) {
		t.Fatal("database inode replaced")
	}
	if got := countRows(t, live); got != [2]int{2, 4} {
		t.Fatalf("rows %v", got)
	}
}

func TestImportLiveSessionDatabaseRefusedRestoreIsReportedAndLeavesDbIntact(t *testing.T) {
	_, live, zipPath := prepareLiveSessionImport(t)
	old := safeRestoreDBFn
	safeRestoreDBFn = func(context.Context, string, string) (bool, error) { return false, nil }
	t.Cleanup(func() { safeRestoreDBFn = old })
	code, out := runImport(t, zipPath, true)
	if code != 1 || !strings.Contains(out, "files skipped") || !strings.Contains(out, "state.db") || strings.Contains(out, "has been restored") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := countRows(t, live); got != [2]int{3, 12} {
		t.Fatalf("rows %v", got)
	}
}

func TestImportLiveSessionDatabaseSidecarMembersAreNotInstalledBesideARestoredDb(t *testing.T) {
	h, live, _ := prepareLiveSessionImport(t)
	staged := filepath.Join(filepath.Dir(h), "backup-state.db")
	zipPath := filepath.Join(filepath.Dir(h), "sidecars.zip")
	raw, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	payloads := map[string]string{"-wal": "foreign-wal-from-archive", "-shm": "foreign-shm", "-journal": "foreign-journal"}
	writeZip(t, zipPath, pyzip.Stored, zmember{"state.db", string(raw)},
		zmember{"state.db-wal", payloads["-wal"]}, zmember{"state.db-shm", payloads["-shm"]}, zmember{"state.db-journal", payloads["-journal"]})
	runImport(t, zipPath, true)
	suffixes := []string{"-wal", "-shm", "-journal"}
	sort.Strings(suffixes)
	for _, suffix := range suffixes {
		if raw, err := os.ReadFile(live + suffix); err == nil && string(raw) == payloads[suffix] {
			t.Fatalf("archive sidecar %s installed", suffix)
		}
	}
	if got := countRows(t, live); got != [2]int{2, 4} {
		t.Fatalf("rows %v", got)
	}
}

func TestImportLiveSessionDatabaseMissingTargetTakesThePlainPublish(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	os.Mkdir(h, 0o755)
	useHomes(t, h, tmp)
	noRealGatewayService(t, false, nil)
	staged := filepath.Join(tmp, "backup-state.db")
	writeSessionDB(t, staged, 2, 3)
	zipPath := filepath.Join(tmp, "backup.zip")
	writeZipFiles(t, zipPath, [2]string{staged, "state.db"})
	runImport(t, zipPath, true)
	if got := countRows(t, filepath.Join(h, "state.db")); got != [2]int{2, 6} {
		t.Fatalf("rows %v", got)
	}
}
