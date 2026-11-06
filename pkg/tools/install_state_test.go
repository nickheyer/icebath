package tools

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

// Swaps a package value for the test and restores it afterwards
func swapForTest[T any](t *testing.T, target *T, value T) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

// Places of a test machine with fixed roots and no protected folders
func testPlaces(markers []string, roots ...string) InstallPlaces {
	return InstallPlaces{Markers: markers, Sources: []InstallSource{installActiveRoot, func(yield func(string) bool) {
		for _, r := range roots {
			if !yield(r) {
				return
			}
		}
	}}}
}

// An empty active root plus one other root the probe is told about, with the marker in the test directory
func installMachine(t *testing.T) (string, InstallPlaces) {
	t.Helper()
	tmp := t.TempDir()
	active, other := filepath.Join(tmp, "active"), filepath.Join(tmp, "other-root")
	for _, d := range []string{active, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", tmp)
	t.Setenv(home.EnvHome, active)
	places := testPlaces([]string{filepath.Join(tmp, "state", ".machine")}, other)
	swapForTest(t, &machinePlaces, func() InstallPlaces { return places })
	InstallStateProbeClear()
	t.Cleanup(InstallStateProbeClear)
	return other, places
}

func installWriteFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func execSQLite(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range statements {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
}

func realDir(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var installUses = map[InstallEvidence]func(t *testing.T, root string){
	EvidenceOnboardingDone: func(t *testing.T, r string) {
		installWriteFile(t, filepath.Join(r, "config.yaml"), "onboarding:\n  run: false\n")
	},
	EvidenceNamedProfile: func(t *testing.T, r string) {
		installWriteFile(t, filepath.Join(r, "profiles", "work", "config.yaml"), "{}\n")
	},
	EvidenceProviderConfig: func(t *testing.T, r string) {
		installWriteFile(t, filepath.Join(r, "config.yaml"), "model:\n  provider: openrouter\n")
	},
	EvidenceProviderKey: func(t *testing.T, r string) {
		installWriteFile(t, filepath.Join(r, ".env"), "OPENROUTER_API_KEY=sk-test\n")
	},
	EvidenceProviderLogin: func(t *testing.T, r string) {
		installWriteFile(t, filepath.Join(r, "auth.json"), `{"providers": {"nous": {"auth_method": "anonymous", "anon_token": "anon_x"}}}`)
	},
	EvidenceMessages: func(t *testing.T, r string) {
		execSQLite(t, filepath.Join(r, "state.db"), "CREATE TABLE sessions (id TEXT PRIMARY KEY)",
			"CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, content TEXT)",
			"INSERT INTO messages (session_id, content) VALUES ('s', 'hi')")
	},
}

func TestInstallStateOnlyInstallTimeFilesIsFresh(t *testing.T) {
	other, places := installMachine(t)
	installWriteFile(t, filepath.Join(other, "config.yaml"), "_config_version: 50\nmodel:\n  default: x\n  provider: auto\n")
	installWriteFile(t, filepath.Join(other, ".env"), "TERMINAL_ENV=local\n# OPENROUTER_API_KEY=\nOPENROUTER_API_KEY=\"\" # optional\n")
	installWriteFile(t, filepath.Join(other, "auth.json"), `{"credential_pool": {"openrouter": [{"source": "env:OPENROUTER_API_KEY"}]}}`)
	execSQLite(t, filepath.Join(other, "state.db"), "CREATE TABLE messages (id INTEGER)")
	installWriteFile(t, filepath.Join(other, "installs", "k", "bootstrap", "default.json"), "{}")

	if !InstallStateProbe().Fresh() {
		t.Fatalf("install time files counted as use: %+v", InstallStateProbe().Findings)
	}
	if _, err := os.Stat(places.Markers[0]); err == nil {
		t.Fatal("a fresh probe wrote the marker")
	}
}

func TestInstallStateUseInAnotherRootMarksTheMachine(t *testing.T) {
	for evidence, use := range installUses {
		t.Run(string(evidence), func(t *testing.T) {
			other, places := installMachine(t)
			use(t, other)

			got := InstallStateProbe().Findings
			want := []InstallFinding{{Evidence: evidence, Where: realDir(t, other)}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("findings %+v, want %+v", got, want)
			}
			if !installIsFile(places.Markers[0]) {
				t.Fatal("use did not mark the machine")
			}
			if err := os.RemoveAll(other); err != nil {
				t.Fatal(err)
			}
			InstallStateProbeClear()
			if f := InstallStateProbe().Findings; len(f) == 0 || f[0].Evidence != EvidenceMachineMarker {
				t.Fatalf("marker did not answer once the root was gone: %+v", f)
			}
		})
	}
}

func TestInstallStateUnreadableRootCountsButNeverMarks(t *testing.T) {
	other, places := installMachine(t)
	installWriteFile(t, filepath.Join(other, "config.yaml"), "model: [unclosed\n")

	st := InstallStateProbe()
	if !st.Existing() || st.Findings[0].Evidence != EvidenceUnreadable {
		t.Fatalf("findings %+v, want unreadable", st.Findings)
	}
	if _, err := os.Stat(places.Markers[0]); err == nil {
		t.Fatal("doubt wrote the marker")
	}
}

func TestInstallStateRootInProtectedFolderIsExistingUnopened(t *testing.T) {
	_, places := installMachine(t)
	protected := filepath.Join(t.TempDir(), "Documents")
	swapForTest(t, &machinePlaces, func() InstallPlaces {
		return InstallPlaces{Markers: places.Markers, Protected: []string{protected},
			Sources: []InstallSource{func(yield func(string) bool) { yield(filepath.Join(protected, "hermes-home")) }}}
	})
	if f := InstallStateProbe().Findings; len(f) != 1 || f[0].Evidence != EvidenceUnreadable {
		t.Fatalf("findings %+v, want unreadable", f)
	}
}

func TestInstallStateFileLinkedIntoProtectedFolderIsUnreadable(t *testing.T) {
	for _, name := range []string{"config.yaml", ".env", "auth.json", "state.db", "profiles"} {
		t.Run(name, func(t *testing.T) {
			other, places := installMachine(t)
			protected := filepath.Join(t.TempDir(), "Documents")
			target := filepath.Join(protected, "hermes", name)
			switch name {
			case "profiles":
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
			case "state.db":
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					t.Fatal(err)
				}
				execSQLite(t, target, "CREATE TABLE messages (id INTEGER)")
			default:
				installWriteFile(t, target, "{}\n")
			}
			if err := os.Symlink(target, filepath.Join(other, name)); err != nil {
				t.Fatal(err)
			}
			swapForTest(t, &machinePlaces, func() InstallPlaces {
				return InstallPlaces{Markers: places.Markers, Sources: places.Sources, Protected: []string{protected}}
			})
			got := InstallStateProbe().Findings
			want := []InstallFinding{{Evidence: EvidenceUnreadable, Where: realDir(t, other)}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("findings %+v, want %+v", got, want)
			}
		})
	}
}

func TestInstallStateLauncherInProtectedFolderNamesItsFolderUnopened(t *testing.T) {
	protected := filepath.Join(t.TempDir(), "Documents")
	launcher := filepath.Join(protected, "hermes-src", "hermes")
	swapForTest(t, &macPrivacyProtectedDirs, func() []string { return []string{protected} })
	swapForTest(t, &installLocateCommand, func(name string, _ *commandLookupContext, _ []string) commandResolution {
		if name != "hermes" {
			return commandResolution{Kind: "missing", Candidates: []commandCandidate{{Value: name, Source: "PATH"}}}
		}
		return commandResolution{Kind: "path_executable", Candidates: []commandCandidate{{Value: launcher, Source: "PATH", Present: true}}}
	})
	swapForTest(t, &installLauncherPython, func(string) (string, bool) {
		t.Fatal("the launcher inside a protected folder was opened")
		return "", false
	})
	var got []string
	installLauncherRoot(func(r string) bool { got = append(got, r); return true })
	if want := []string{filepath.Dir(launcher)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("launcher roots %v, want %v", got, want)
	}
}

func TestInstallStateClaimMarksTheMachine(t *testing.T) {
	installMachine(t)
	if !InstallStateProbe().Fresh() {
		t.Fatal("empty machine was not fresh")
	}
	if err := MarkMachineUsed(); err != nil {
		t.Fatal(err)
	}
	if !ExistingInstall() {
		t.Fatal("the next probe did not see the claim")
	}
}

func TestInstallStateReservationIsReadAndConsumed(t *testing.T) {
	_, places := installMachine(t)
	if TrialReserved() {
		t.Fatal("a missing marker reads as reserved")
	}
	if err := ReserveInstallTrial(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(places.Markers[0]); string(raw) != "{\n  \"trial\": \"reserved\"\n}" {
		t.Fatalf("reservation marker %q", raw)
	}
	if !TrialReserved() || !ExistingInstall() {
		t.Fatal("a reservation must be reserved and make the machine read used")
	}
	if err := MarkMachineUsed(); err != nil {
		t.Fatal(err)
	}
	if TrialReserved() {
		t.Fatal("a claim did not consume the reservation")
	}
	if raw, _ := os.ReadFile(places.Markers[0]); string(raw) != "{}" {
		t.Fatalf("claim marker %q", raw)
	}
}

func TestInstallStateClaimLockIsExclusiveAndReentrant(t *testing.T) {
	_, places := installMachine(t)
	release, err := ClaimLock()
	if err != nil {
		t.Fatal(err)
	}
	inner, err := ClaimLock()
	if err != nil {
		t.Fatalf("the holding goroutine could not re-enter: %v", err)
	}
	inner()
	if raw, _ := os.ReadFile(places.Markers[0] + ".lock"); strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock file %q does not name this pid", raw)
	}
	done := make(chan error, 1)
	go func() {
		_, err := acquireFileLock(places.Markers[0]+".lock", &fileLockHolder{}, time.Second, "Timed out waiting for the welcome-trial claim lock")
		done <- err
	}()
	err = <-done
	if err == nil || !strings.HasPrefix(err.Error(), "Timed out waiting for the welcome-trial claim lock") {
		t.Fatalf("a second holder was not refused: %v", err)
	}
	release()
	again, err := ClaimLock()
	if err != nil {
		t.Fatalf("the released lock could not be taken again: %v", err)
	}
	again()
}

func isolateServiceTables(t *testing.T) {
	t.Helper()
	swapForTest(t, &gatewayServiceDefinitionGlobs, func() []string { return nil })
	swapForTest(t, &systemGatewayServiceDefinitionGlobs, func() []string { return nil })
	swapForTest(t, &installTaskLaunchers, func() []string { return nil })
}

func collectServiceHomes() map[string]bool {
	homes := map[string]bool{}
	installServiceHomes(func(h string) bool { homes[h] = true; return true })
	return homes
}

func TestInstallStateGatewayServiceDefinitionsNameTheirHomes(t *testing.T) {
	isolateServiceTables(t)
	tmp := t.TempDir()
	gw := filepath.Join(tmp, "gw-home")
	installWriteFile(t, filepath.Join(tmp, "hermes-gateway.service"), "[Service]\nEnvironment=\"HERMES_HOME="+gw+"\"\n")
	installWriteFile(t, filepath.Join(tmp, "Hermes_Gateway.vbs"), "env.Item(\"HERMES_HOME\") = \""+gw+"-win\"\r\n")
	installWriteFile(t, filepath.Join(tmp, "icebath-gateway.service"), "[Service]\nEnvironment=\"ICEBATH_HOME="+gw+"-ice\"\n")
	swapForTest(t, &gatewayServiceDefinitionGlobs, func() []string {
		return []string{filepath.Join(tmp, "*Gateway*"), filepath.Join(tmp, "*gateway*")}
	})
	want := map[string]bool{gw: true, gw + "-win": true, gw + "-ice": true}
	if got := collectServiceHomes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("service homes %v, want %v", got, want)
	}
}

// UTF-16 bytes with a little endian byte order mark, the way the Windows installer writes launchers
func utf16BOM(text string) []byte {
	var b bytes.Buffer
	b.Write([]byte{0xff, 0xfe})
	for _, u := range utf16.Encode([]rune(text)) {
		_ = binary.Write(&b, binary.LittleEndian, u)
	}
	return b.Bytes()
}

// The home's gateway.vbs launcher as gateway_windows writes it, naming its home
func writeGatewayVBS(t *testing.T, gwHome string) string {
	t.Helper()
	script := filepath.Join(gwHome, "gateway-service", "Hermes_Gateway.vbs")
	lines := []string{"' Hermes Agent Gateway - Messaging Platform Integration", "Option Explicit", "Dim sh, env, existing_pp",
		`Set sh = CreateObject("WScript.Shell")`, `Set env = sh.Environment("PROCESS")`,
		`env.Item("HERMES_HOME") = "` + gwHome + `"`, `env.Item("PYTHONIOENCODING") = "utf-8"`,
		`sh.CurrentDirectory = "` + gwHome + `"`, `sh.Run """python.exe"" -m hermes_cli.main gateway run", 0, False`}
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, utf16BOM(strings.Join(lines, "\r\n")+"\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestInstallStateScheduledTaskAndStartupFallbackNameTheirHomes(t *testing.T) {
	isolateServiceTables(t)
	tmp := t.TempDir()
	byTask, byStartup := filepath.Join(tmp, "task home 方舟"), filepath.Join(tmp, "startup home Usuário")
	taskLauncher := writeGatewayVBS(t, byTask)
	target := writeGatewayVBS(t, byStartup)
	startup := filepath.Join(tmp, "Startup", "Hermes_Gateway.vbs")
	entry := []string{"' Hermes Agent Gateway - Messaging Platform Integration", "Option Explicit", "Dim fso, sh, target",
		`target = "` + target + `"`, `Set fso = CreateObject("Scripting.FileSystemObject")`,
		"If Not fso.FileExists(target) Then WScript.Quit 0", `Set sh = CreateObject("WScript.Shell")`,
		`sh.Run "wscript.exe ""` + target + `""", 0, False`}
	if err := os.MkdirAll(filepath.Dir(startup), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(startup, utf16BOM(strings.Join(entry, "\r\n")+"\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	swapForTest(t, &gatewayServiceDefinitionGlobs, func() []string { return []string{filepath.Join(tmp, "Startup", "Hermes_Gateway*")} })
	swapForTest(t, &installTaskLaunchers, func() []string { return []string{taskLauncher} })

	homes := collectServiceHomes()
	if !homes[byTask] || !homes[byStartup] {
		t.Fatalf("service homes %v miss %q or %q", homes, byTask, byStartup)
	}
}

func TestInstallStateLockedStateDBIsNotUse(t *testing.T) {
	other, _ := installMachine(t)
	db := filepath.Join(other, "state.db")
	execSQLite(t, db, "CREATE TABLE messages (id INTEGER)")
	swapForTest(t, &installDBBusy, 50*time.Millisecond)
	holder, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	holder.SetMaxOpenConns(1)
	for _, s := range []string{"PRAGMA journal_mode=DELETE", "BEGIN EXCLUSIVE"} {
		if _, err := holder.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	defer holder.Exec("ROLLBACK")
	if st := InstallStateProbe(); !st.Fresh() {
		t.Fatalf("a held lock read as use: %+v", st.Findings)
	}
}

func TestRegisteredTaskLaunchersReadEachGatewayTaskAction(t *testing.T) {
	launcher := `C:\Users\Ann Lee\custom hermes\gateway-service\Hermes_Gateway_work.vbs`
	legacy := `C:\Users\ann\h2\gateway-service\Hermes_Gateway.cmd`
	iceLauncher := `C:\Users\ann\ice\gateway-service\Icebath_Gateway.vbs`
	taskXML := func(command, arguments string) string {
		return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Hermes Agent Gateway - Messaging Platform Integration</Description>
  </RegistrationInfo>
  <Actions Context="Author">
    <Exec>
      <Command>` + command + `</Command>
      <Arguments>` + arguments + `</Arguments>
    </Exec>
  </Actions>
</Task>`
	}
	current := taskXML("wscript.exe", `//B //Nologo "`+launcher+`"`)
	older := taskXML(legacy, "")
	listing := "\r\n\"\\Microsoft\\Windows\\Defrag\\ScheduledDefrag\",\"N/A\",\"Ready\"\r\n" +
		"\"\\Hermes_Gateway_work\",\"N/A\",\"Ready\"\r\n\"\\Hermes_Gateway\",\"N/A\",\"Running\"\r\n" +
		"\"\\Icebath_Gateway\",\"N/A\",\"Ready\"\r\n"
	tasks := map[string]string{"Hermes_Gateway_work": current, "Hermes_Gateway": older,
		"Icebath_Gateway": taskXML("wscript.exe", `//B //Nologo "`+iceLauncher+`"`)}
	run := func(args []string, _ time.Duration) (int, string, string) {
		if slicesContains(args, "/FO") {
			return 0, listing, ""
		}
		return 0, tasks[args[2]], ""
	}
	want := []string{launcher, legacy, iceLauncher}
	if got := taskLaunchersFrom(run); !reflect.DeepEqual(got, want) {
		t.Fatalf("task launchers %q, want %q", got, want)
	}
	if runtimeIsWindows := isWindowsHost; !runtimeIsWindows && registeredTaskLaunchers() != nil {
		t.Fatal("task launchers are only registered on Windows")
	}
}

func slicesContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestDecodeSchtasksOutputReadsANSIUnderUTF8(t *testing.T) {
	xmlText := `<Arguments>//B //Nologo "C:\Users\方舟\AppData\Local\hermes\gateway-service\Hermes_Gateway.vbs"</Arguments>`
	gbk := map[rune][]byte{'方': {0xb7, 0xbd}, '舟': {0xd6, 0xdb}}
	var raw []byte
	for _, r := range xmlText {
		if b, ok := gbk[r]; ok {
			raw = append(raw, b...)
		} else {
			raw = append(raw, byte(r))
		}
	}
	got, err := decodeSchtasksOutput(raw, []int{936}, pyCodePageDecoder)
	if err != nil || got != xmlText {
		t.Fatalf("decoded %q %v, want %q", got, err, xmlText)
	}
}

func TestDecodeSchtasksOutputHonoursUTF16BOM(t *testing.T) {
	text := `<Arguments>C:\Users\方舟</Arguments>`
	if got, _ := decodeSchtasksOutput(utf16BOM(text), nil, pyCodePageDecoder); got != text {
		t.Fatalf("decoded %q, want %q", got, text)
	}
	if got, _ := decodeSchtasksOutput([]byte("\xff\xfe<\x00?"), nil, pyCodePageDecoder); got != "\ufffd\ufffd<\x00?" {
		t.Fatalf("malformed UTF-16 decoded %q", got)
	}
}

func TestTaskXMLLeafValuesIsNamespaceAgnostic(t *testing.T) {
	leaves := taskXMLLeafValues("\ufeff<?xml version=\"1.0\" encoding=\"UTF-16\"?><t:Task version=\"1.2\" xmlns:t=\"u\"><t:A>x<!--c-->y <![CDATA[z]]></t:A><t:B/></t:Task>")
	want := map[string]string{"Task@version": "1.2", "Task/A": "xy z", "Task/B": ""}
	if !reflect.DeepEqual(leaves, want) {
		t.Fatalf("leaves %v, want %v", leaves, want)
	}
	for _, bad := range []string{"<a><b>1</b></a>junk", "<x:a/>", "<a", ""} {
		if taskXMLLeafValues(bad) != nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestLauncherPythonReadsEachPublishedLayout(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "gone", "python.exe")
	var payload bytes.Buffer
	archive := zip.NewWriter(&payload)
	w, err := archive.Create("__main__.py")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("raise SystemExit(0)\n")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	exe := append([]byte("MZ"), make([]byte, 64)...)
	exe = append(exe, "#!"+missing+" -I"...)
	exe = append(exe, payload.Bytes()...)
	installWriteFile(t, filepath.Join(tmp, "hermes.exe"), string(exe))
	installWriteFile(t, filepath.Join(tmp, "hermes.cmd"), "@echo off\r\n\r\n\"C:\\Program Files\\h\\tools\\python\\python.exe\" -I -c \"eA==\" %*\r\n")
	installWriteFile(t, filepath.Join(tmp, "hermes"), "#!/bin/sh\nexec '/opt/it'\"'\"'s/tools/python/bin/python3' -I -c '\nimport x\n' \"$@\"\n")
	installWriteFile(t, filepath.Join(tmp, "hashed"), "#!/bin/sh\nexec /opt/py#thon -I\n")
	cases := map[string]string{"hermes.exe": missing, "hermes.cmd": `C:\Program Files\h\tools\python\python.exe`,
		"hermes": "/opt/it's/tools/python/bin/python3", "hashed": "/opt/py"}
	for name, want := range cases {
		if got, ok := launcherPython(filepath.Join(tmp, name)); !ok || got != want {
			t.Fatalf("%s: launcher python %q %v, want %q", name, got, ok, want)
		}
	}
	installWriteFile(t, filepath.Join(tmp, "plain.exe"), "MZ not a zip")
	if _, ok := launcherPython(filepath.Join(tmp, "plain.exe")); ok {
		t.Fatal("an exe without a zip named an interpreter")
	}
}

func TestShlexFirstTokenMatchesPosixShlex(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		present bool
		err     bool
	}{
		{"/usr/bin/python3 -I", "/usr/bin/python3", true, false},
		{`'a b'"c"\ d rest`, "a bc d", true, false},
		{`"a\"b\x"`, `a"b\x`, true, false},
		{"''", "", true, false},
		{"   ", "", false, false},
		{"# only comment", "", false, false},
		{"'unclosed", "", false, true},
		{`trailing\`, "", false, true},
	}
	for _, c := range cases {
		got, present, err := shlexPosixFirstToken(c.in)
		if got != c.want || present != c.present || (err != nil) != c.err {
			t.Fatalf("%q: token %q present %v err %v", c.in, got, present, err)
		}
	}
}

func TestInstallExpandAllDropsUnsetVariablesAndGlobs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_STATE_HOME", "")
	os.Unsetenv("XDG_STATE_HOME")
	for _, d := range []string{".icebath", ".icebath-canary", ".hermes", ".other"} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := slicesCollect(installExpandAll([]string{"$XDG_STATE_HOME/nous-research/.machine", "~/.local/state/nous-research/.machine", "~/.icebath*", "~/.hermes*", "~/*"}))
	want := []string{filepath.Join(tmp, ".local", "state", "nous-research", ".machine"), filepath.Join(tmp, ".icebath"),
		filepath.Join(tmp, ".icebath-canary"), filepath.Join(tmp, ".hermes")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded %q, want %q", got, want)
	}
}

func slicesCollect(src InstallSource) []string {
	var out []string
	src(func(s string) bool { out = append(out, s); return true })
	return out
}

func TestHermesYAMLSafeLoadFollowsTheYAML11Policy(t *testing.T) {
	cases := []struct {
		doc            string
		onboardingDone bool
		providerSet    bool
		err            bool
	}{
		{"onboarding:\n  run: false\n", true, false, false},
		{"onboarding:\n  run: Off\n", true, false, false},
		{"onboarding:\n  run: no\n", true, false, false},
		{"onboarding:\n  run: n\n", true, false, false},
		{"onboarding:\n  run: oFF\n", false, false, false},
		{"onboarding:\n  run: 'off'\n", false, false, false},
		{"onboarding:\n  run: ! off\n", true, false, false},
		{"onboarding:\n  run: ! 'off'\n", true, false, false},
		{"onboarding:\n  run: ! &x 'off'\n", true, false, false},
		{"onboarding:\n  run: &x ! \"off\"\n", true, false, false},
		{"onboarding:\n  run: !\n    'off'\n", true, false, false},
		{"onboarding:\n  run: ! # c\n    'off'\n", true, false, false},
		{"onboarding:\n  run: ! |\n    off\n", false, false, true},
		{"onboarding: {run: ! 'off'}\n", true, false, false},
		{"onboarding: {ключ: 1, run: ! 'off'}\r\n", true, false, false},
		{"- x !\n- 'off'\n", false, false, false},
		{"onboarding:\n  run: !!bool NO\n", true, false, false},
		{"onboarding:\n  run: 0\n", false, false, false},
		{"base: &b {run: false}\nonboarding:\n  <<: *b\n", true, false, false},
		{"onboarding:\n  <<: [{run: false}, {run: true}]\n", true, false, false},
		{"onboarding:\n  run: true\n  <<: {run: false}\n", false, false, false},
		{"onboarding: !!omap [run: false]\n", true, false, false},
		{"%YAML 1.2\n---\nonboarding:\n  run: off\n", true, false, false},
		{"%YAML 1.1\n---\nonboarding:\n  run: off\n", true, false, false},
		{"model:\n  provider: openrouter\n", false, true, false},
		{"model:\n  provider: auto\n", false, false, false},
		{"model:\n  provider: ' auto '\n", false, false, false},
		{"model:\n  provider: yes\n", false, false, false},
		{"model:\n  provider: 2024-01-01\n", false, false, false},
		{"model:\n  provider: 1:30\n", false, false, false},
		{"model:\n  provider: 1:70\n", false, true, false},
		{"model:\n  provider: 09\n", false, true, false},
		{"model:\n  provider: .5e3\n", false, true, false},
		{"model:\n  provider: x\n  provider: y\n", false, false, true},
		{"a: 1\ntrue: b\n1: c\n", false, false, true},
		{"1.0: a\n1: b\n", false, false, true},
		{".nan: a\n.nan: b\n", false, false, true},
		{"2024-01-01: a\n2024-01-01 00:00:00: b\n", false, false, false},
		{"a: 1\n---\nb: 2\n", false, false, true},
		{"a: !foo x\n", false, false, true},
		{"a: =\n", false, false, true},
		{"a: <<\n", false, false, true},
		{"a: 0b_\n", false, false, true},
		{"a: 2024-13-45\n", false, false, true},
		{"a: 2024-02-30\n", false, false, true},
		{"a: 2024-01-01 1:00:00 +24\n", false, false, true},
		{"a: !!binary aGk\n", false, false, true},
		{"a: !!binary 'a GVs bG8='\n", false, false, false},
		{"a: !!bool maybe\n", false, false, true},
		{"a: !!int ''\n", false, false, true},
		{"a: !!float x\n", false, false, true},
		{"a: !!set [1]\n", false, false, true},
		{"a: !!omap [a: 1, a: 2]\n", false, false, true},
		{"{a: 1}: x\n", false, false, true},
		{"[[1]]: x\n", false, false, true},
		{"[1, 2]: x\n", false, false, false},
		{"<<: {a: 2}\n<<: {b: 1}\n", false, false, true},
		{"a: 1\na: 1\n<<: {}\n", false, false, true},
		{"x:\n  <<: [1]\n", false, false, true},
		{"%YAML 2.0\n---\na: 1\n", false, false, true},
		{"\ta: 1\n", false, false, true},
		{"", false, false, false},
		{"# c\n", false, false, false},
		{"- 1\n", false, false, false},
		{"model: {provider: http://h}\n", false, true, false},
	}
	for _, c := range cases {
		doc, err := hermesYAMLSafeLoad(c.doc)
		if (err != nil) != c.err {
			t.Fatalf("%q: error %v, want error %v", c.doc, err, c.err)
		}
		if err != nil {
			continue
		}
		cfg, _ := doc.(map[string]any)
		r := &installRoot{configRead: true, config: cfg}
		done, _ := installOnboardingDone(r)
		provider, _ := installProviderConfig(r)
		if done != c.onboardingDone || provider != c.providerSet {
			t.Fatalf("%q: onboarding done %v provider %v, want %v %v", c.doc, done, provider, c.onboardingDone, c.providerSet)
		}
	}
}

// Points a probe at one root under a temp HOME, the way the hermes conftest isolates the machine marker
func onboardingRoot(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "active")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, root)
	t.Setenv(config.EnvManagedDir, filepath.Join(tmp, "no-managed"))
	config.InvalidateManagedCache()
	config.ResetCache()
	places := InstallPlaces{Markers: []string{filepath.Join(tmp, "machine-marker")}, Sources: []InstallSource{installActiveRoot}}
	swapForTest(t, &machinePlaces, func() InstallPlaces { return places })
	InstallStateProbeClear()
	OnboardingDueResetForTests()
	t.Cleanup(func() {
		InstallStateProbeClear()
		config.InvalidateManagedCache()
		config.ResetCache()
	})
	return root
}

func addChat(t *testing.T, h string) {
	t.Helper()
	db, err := state.Open(home.StatePath(h), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: "old-session", Source: "desktop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage("old-session", &state.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
}

func createProfile(t *testing.T, root, name string) string {
	t.Helper()
	p := filepath.Join(root, home.ProfilesDir, name)
	installWriteFile(t, home.ConfigPath(p), "{}\n")
	return p
}

func TestOnboardingRunsOnAFreshInstall(t *testing.T) {
	root := onboardingRoot(t)
	db, err := state.Open(home.StatePath(root), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if !OnboardingShouldRun(false) {
		t.Fatal("a fresh install with an empty state.db did not run onboarding")
	}
}

func TestOnboardingEmptySessionRowIsNotUse(t *testing.T) {
	root := onboardingRoot(t)
	db, err := state.Open(home.StatePath(root), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "minted", Source: "desktop"}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if !OnboardingShouldRun(false) {
		t.Fatal("a session row without a message counted as use")
	}
}

func TestOnboardingRootWithAChatDoesNotRun(t *testing.T) {
	root := onboardingRoot(t)
	addChat(t, root)
	if OnboardingShouldRun(false) {
		t.Fatal("a root with a chat ran onboarding")
	}
}

func TestOnboardingUserProfileIsHistory(t *testing.T) {
	root := onboardingRoot(t)
	createProfile(t, root, "work")
	if OnboardingShouldRun(false) {
		t.Fatal("a created profile did not count as history")
	}
}

func TestOnboardingManagedRunFalseWinsOnAFreshInstall(t *testing.T) {
	onboardingRoot(t)
	managed := filepath.Join(t.TempDir(), "managed")
	installWriteFile(t, filepath.Join(managed, "config.yaml"), "onboarding:\n  run: false\n")
	t.Setenv(config.EnvManagedDir, managed)
	config.InvalidateManagedCache()
	if OnboardingShouldRun(false) {
		t.Fatal("the managed run false did not win")
	}
}

func TestOnboardingExplicitTrueRunsDespiteHistory(t *testing.T) {
	root := onboardingRoot(t)
	addChat(t, root)
	if err := SetOnboardingRun(true, false); err != nil {
		t.Fatal(err)
	}
	if !OnboardingShouldRun(false) {
		t.Fatal("explicit run true did not win over history")
	}
}

func TestOnboardingSetRunFalsePersistsAndKeepsComments(t *testing.T) {
	root := onboardingRoot(t)
	installWriteFile(t, home.ConfigPath(root), "# my notes\nmodel:\n  default: some-model  # pinned\n")
	if err := SetOnboardingRun(false, false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(home.ConfigPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# my notes") || !strings.Contains(string(raw), "# pinned") {
		t.Fatalf("comments lost: %q", raw)
	}
	doc, err := config.ReadUser(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc["model"], map[string]any{"default": "some-model"}) {
		t.Fatalf("model %v", doc["model"])
	}
	if OnboardingShouldRun(false) {
		t.Fatal("run false did not persist")
	}
}

func TestOnboardingNamedProfileLaunchUsesTheRootFile(t *testing.T) {
	root := onboardingRoot(t)
	work := createProfile(t, root, "work")
	t.Setenv(home.EnvHome, work)
	if home.Root() != root {
		t.Fatalf("root %q, want %q", home.Root(), root)
	}
	if err := SetOnboardingRun(true, false); err != nil {
		t.Fatal(err)
	}
	rootDoc, _ := config.ReadUser(root)
	if section, _ := rootDoc["onboarding"].(map[string]any); section["run"] != true {
		t.Fatalf("root onboarding %v", rootDoc["onboarding"])
	}
	workDoc, _ := config.ReadUser(work)
	if section, _ := workDoc["onboarding"].(map[string]any); section != nil {
		if _, ok := section["run"]; ok {
			t.Fatal("the profile file got the root flag")
		}
	}
	if !OnboardingShouldRun(false) {
		t.Fatal("the root flag did not answer for the profile launch")
	}
	installWriteFile(t, home.ConfigPath(work), "onboarding:\n  run: true\n")
	if err := SetOnboardingRun(false, false); err != nil {
		t.Fatal(err)
	}
	if OnboardingShouldRun(false) {
		t.Fatal("the profile file overrode the root flag")
	}
}

func TestOnboardingLatchFirstInstallReservesTheTrialOnce(t *testing.T) {
	onboardingRoot(t)
	latched, err := OnboardingLatchFirstInstall()
	if err != nil || !latched {
		t.Fatalf("a fresh install did not latch: %v %v", latched, err)
	}
	if !TrialReserved() || !OnboardingShouldRun(false) {
		t.Fatal("the latch did not reserve the trial and keep onboarding due")
	}
	if again, err := OnboardingLatchFirstInstall(); err != nil || again {
		t.Fatalf("the latch fired twice: %v %v", again, err)
	}
}

func TestPathlibHelpersFollowPurePath(t *testing.T) {
	slash := filepath.FromSlash
	if got := pathlibParents("a//b/./c/"); !reflect.DeepEqual(got, []string{slash("a/b"), "a", "."}) {
		t.Fatalf("parents %q", got)
	}
	if got := pathlibParents("/a/../b"); !reflect.DeepEqual(got, []string{slash("/a/.."), slash("/a"), slash("/")}) {
		t.Fatalf("parents %q", got)
	}
	if pathlibName("/a/tools/") != "tools" || pathlibParent(slash("/a/tools")) != slash("/a") {
		t.Fatal("name and parent")
	}
	if pathlibSuffix("a.b.CMD") != ".CMD" || pathlibSuffix(".exe") != "" || pathlibSuffix("a.") != "." {
		t.Fatal("suffix")
	}
}
