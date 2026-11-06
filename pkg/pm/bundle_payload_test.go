package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func bpHostMinor(t *testing.T, python string) string {
	t.Helper()
	out, err := exec.Command(python, "-c", "import sys; print('%d.%d' % sys.version_info[:2])").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func bpReadJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(readFile(t, path), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func bpMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestBundleWrapperRejectsUnresolvedTemplateFields(t *testing.T) {
	original := launcherWrapperTemplate
	t.Cleanup(func() { launcherWrapperTemplate = original })
	launcherWrapperTemplate = "entry = \"__ICEBATH_ENTRY_MODULE__\"\nmissing = \"__ICEBATH_NEW__\"\n"
	if _, err := renderWrapper("entry:run", "../app", "../venv/Lib/site-packages"); err == nil || !strings.Contains(err.Error(), "__ICEBATH_NEW__") {
		t.Fatalf("unresolved placeholder accepted: %v", err)
	}
}

func TestBundleRelocationPreservesSiblingAndFrameworkLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix links")
	}
	root := filepath.Join(t.TempDir(), "payload")
	store, venv := filepath.Join(root, "tools/python/bin"), filepath.Join(root, "venv/bin")
	writeFile(t, filepath.Join(store, "python3"), []byte("interpreter"), 0o666)
	os.MkdirAll(venv, 0o777)
	os.Symlink("/builder/tools/python/bin/python3", filepath.Join(venv, "python"))
	os.Symlink("python", filepath.Join(venv, "python3"))
	framework := filepath.Join(root, "tools/framework")
	os.Symlink("python/bin/python3", framework)
	if count, err := relativizeLinkCount(root); err != nil || count != 1 {
		t.Fatalf("first relocation %d %v", count, err)
	}
	if string(readFile(t, filepath.Join(venv, "python3"))) != "interpreter" {
		t.Fatal("sibling chain broken")
	}
	if target, _ := os.Readlink(filepath.Join(venv, "python3")); target != "python" {
		t.Fatalf("sibling rewritten to %q", target)
	}
	if target, _ := os.Readlink(framework); target != "python/bin/python3" {
		t.Fatalf("framework rewritten to %q", target)
	}
	if count, err := relativizeLinkCount(root); err != nil || count != 0 {
		t.Fatalf("second relocation %d %v", count, err)
	}
	os.Symlink("/usr/bin/python", filepath.Join(venv, "bad"))
	if err := relativizeLinks(root); err == nil || !strings.Contains(err.Error(), "escapes payload") {
		t.Fatalf("escaping link accepted: %v", err)
	}
}

func TestBundleBothLaunchersKeepActiveHomeAndCallDeclaredFunction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix launchers")
	}
	python := sttHostPython(t)
	for _, target := range []string{"linux-x64", "linux-arm64-bionic"} {
		t.Run(target, func(t *testing.T) {
			tmp := t.TempDir()
			root := filepath.Join(tmp, "payload with spaces")
			os.MkdirAll(filepath.Join(root, "bin"), 0o777)
			os.Symlink(python, filepath.Join(root, "python"))
			writeFile(t, filepath.Join(root, "app/entry.py"), []byte("import json,os,sys\ndef run():\n print(json.dumps([os.environ['ICEBATH_HOME'],sys.argv[1:]])); return 7\n"), 0o666)
			launcher := filepath.Join(root, "bin/custom")
			writeFile(t, launcher, []byte(posixLauncher("custom", "entry:run", "python", "app", "deps", target)), 0o666)
			home := filepath.Join(tmp, "custom/profiles/memory")
			cmd := exec.Command("sh", launcher, "two words", "$(nope)", "")
			cmd.Dir, cmd.Env = tmp, append(os.Environ(), "ICEBATH_HOME="+home)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
				t.Fatalf("exit %v: %s", err, stderr.String())
			}
			var got []any
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want := []any{home, []any{"two words", "$(nope)", ""}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v", got)
			}
		})
	}
}

var bpDocxTemplate = []string{"default.docx", "default-styles.xml", "default-docx-template/[Content_Types].xml",
	"default-docx-template/_rels/.rels", "default-docx-template/word/document.xml"}

func TestBundleMSIXDocxTemplateLeavesVenvAndUVCache(t *testing.T) {
	root := t.TempDir()
	site := "venv/Lib/site-packages/docx/templates"
	cache := "uv-cache/archive-v0/cXkCcX-PYzjQ7QIz/docx/templates"
	for _, base := range []string{site, cache} {
		for _, name := range bpDocxTemplate {
			writeFile(t, filepath.Join(root, filepath.FromSlash(base+"/"+name)), []byte("x"), 0o666)
		}
	}
	contributor := filepath.Join(root, "icebath-agent/contributors/emails/[bot]@example.com")
	writeFile(t, contributor, []byte("x"), 0o666)
	removed, err := dropMSIXReservedNames(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tree := range removed {
		got = append(got, filepath.ToSlash(tree))
	}
	sort.Strings(got)
	if want := []string{cache + "/default-docx-template", site + "/default-docx-template"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("removed %v", got)
	}
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if strings.ToLower(filepath.Base(path)) == "[content_types].xml" {
			t.Fatalf("reserved name kept at %s", path)
		}
		return nil
	})
	for _, base := range []string{site, cache} {
		for _, name := range []string{"default.docx", "default-styles.xml"} {
			if !isRegularFile(filepath.Join(root, filepath.FromSlash(base), name)) {
				t.Fatalf("%s/%s removed", base, name)
			}
		}
	}
	if !isRegularFile(contributor) {
		t.Fatal("unrelated bracketed file removed")
	}
}

func TestBundleMSIXUnknownReservedNameFailsStagingAndNamesTheFile(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "venv/Lib/site-packages/somepkg/data/[Content_Types].XML")
	writeFile(t, file, []byte("x"), 0o666)
	_, err := dropMSIXReservedNames(root)
	var install *InstallError
	if !errors.As(err, &install) || !strings.Contains(err.Error(), "venv/Lib/site-packages/somepkg/data/[Content_Types].XML") {
		t.Fatalf("got %v", err)
	}
	if !isRegularFile(file) {
		t.Fatal("offending file removed")
	}
}

func bpMakeBakePayload(t *testing.T, root, minor string) string {
	t.Helper()
	writeFile(t, filepath.Join(root, "icebath-agent/pkg/__init__.py"), nil, 0o666)
	writeFile(t, filepath.Join(root, "icebath-agent/pkg/mod.py"), []byte("X = 1\n"), 0o666)
	writeFile(t, filepath.Join(root, "venv/lib/python"+minor+"/site-packages/dep.py"), []byte("Y = 2\n"), 0o666)
	writeFile(t, filepath.Join(root, "pm-runtime/lib/python"+minor+"/site-packages/pmdep.py"), []byte("Z = 3\n"), 0o666)
	return root
}

func TestBakeProducesOwnerWritableUncheckedHashPycs(t *testing.T) {
	python := sttHostPython(t)
	root := bpMakeBakePayload(t, t.TempDir(), bpHostMinor(t, python))
	result, err := bakeBytecode(context.Background(), root, python)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(readFile(t, filepath.Join(root, bakedBytecodeMarker)))) != "unchecked-hash" {
		t.Fatal("marker missing")
	}
	pycs, _ := filepath.Glob(filepath.Join(root, "icebath-agent/pkg/__pycache__/mod*.pyc"))
	if len(pycs) == 0 {
		t.Fatal("no pyc")
	}
	if data := readFile(t, pycs[0]); !bytes.Equal(data[4:8], []byte{1, 0, 0, 0}) {
		t.Fatalf("flags %v", data[4:8])
	}
	if bpMode(t, pycs[0])&0o200 == 0 {
		t.Fatal("pyc not owner writable")
	}
	if result["modules"] < 3 {
		t.Fatalf("modules %d", result["modules"])
	}
}

func TestBakeFailsClosedOnUncompilableModule(t *testing.T) {
	python := sttHostPython(t)
	root := bpMakeBakePayload(t, t.TempDir(), bpHostMinor(t, python))
	writeFile(t, filepath.Join(root, "icebath-agent/pkg/broken.py"), []byte("this is (( not python\n"), 0o666)
	var failed *CalledProcessError
	if _, err := bakeBytecode(context.Background(), root, python); !errors.As(err, &failed) {
		t.Fatalf("got %v", err)
	}
}

func TestBakeUncoveredReportsParseableModuleWithoutPyc(t *testing.T) {
	python := sttHostPython(t)
	root := bpMakeBakePayload(t, t.TempDir(), bpHostMinor(t, python))
	if _, err := bakeBytecode(context.Background(), root, python); err != nil {
		t.Fatal(err)
	}
	tag, err := runBakeProbe(context.Background(), python, "tag", nil)
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "icebath-agent")
	if missing, unparseable, err := bakeUncovered(context.Background(), repo, python, tag[0]); err != nil || len(missing) != 0 || unparseable != 0 {
		t.Fatalf("baked tree uncovered %v %d %v", missing, unparseable, err)
	}
	if err := os.Remove(cacheFromSource(filepath.Join(repo, "pkg/mod.py"), tag[0])); err != nil {
		t.Fatal(err)
	}
	missing, _, err := bakeUncovered(context.Background(), repo, python, tag[0])
	if err != nil || len(missing) != 1 || filepath.Base(missing[0]) != "mod.py" {
		t.Fatalf("missing %v %v", missing, err)
	}
}

func TestBakeRejectsMissingImportRoot(t *testing.T) {
	python := sttHostPython(t)
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "venv"), 0o777)
	var py *PyError
	if _, err := bakeBytecode(context.Background(), root, python); !errors.As(err, &py) || py.Class != "FileNotFoundError" {
		t.Fatalf("got %v", err)
	}
}

func bpConfigureEnviron(t *testing.T, python, here string, environ map[string]string) map[string]string {
	t.Helper()
	text, err := renderWrapper("stubmod.entry:main", "../icebath-agent", "../venv/Lib/site-packages")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "wrapper.py")
	writeFile(t, wrapper, []byte(text), 0o666)
	encoded, _ := json.Marshal(environ)
	script := "import json, sys\nns = {'__name__': 'launcher_wrapper_under_test'}\nexec(compile(open(sys.argv[1]).read(), sys.argv[1], 'exec'), ns)\nenviron = json.loads(sys.argv[3])\nns['configure'](sys.argv[2], environ=environ)\nprint(json.dumps(environ))\n"
	out, err := exec.Command(python, "-c", script, wrapper, here, string(encoded)).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestBakeLauncherSkipsUserCacheRedirectWhenMarkerPresent(t *testing.T) {
	python := sttHostPython(t)
	tmp := t.TempDir()
	payload := bpMakeBakePayload(t, tmp, bpHostMinor(t, python))
	writeFile(t, filepath.Join(payload, bakedBytecodeMarker), []byte("unchecked-hash\n"), 0o666)
	here := filepath.Join(payload, "bin")
	os.Mkdir(here, 0o777)
	environ := bpConfigureEnviron(t, python, here, map[string]string{"HOME": filepath.Join(tmp, "userhome")})
	if _, ok := environ["PYTHONPYCACHEPREFIX"]; ok {
		t.Fatalf("redirect set with baked marker: %v", environ)
	}
}

func TestBakeLauncherKeepsUserCacheRedirectWithoutMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix cache home")
	}
	python := sttHostPython(t)
	tmp := t.TempDir()
	payload := bpMakeBakePayload(t, tmp, bpHostMinor(t, python))
	here := filepath.Join(payload, "bin")
	os.Mkdir(here, 0o777)
	environ := bpConfigureEnviron(t, python, here, map[string]string{"HOME": filepath.Join(tmp, "userhome")})
	if environ["PYTHONPYCACHEPREFIX"] != filepath.Join(tmp, "userhome", ".cache", "icebath-pycache") {
		t.Fatalf("redirect %v", environ)
	}
}

func bpDigestPayload(t *testing.T, tmp string) string {
	t.Helper()
	root := filepath.Join(tmp, "payload")
	target, err := CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	lock := NewLockfile(filepath.Join(tmp, "lock.json"))
	entries := NewOrderedObject()
	for _, name := range []string{"python", "uv"} {
		writeFile(t, filepath.Join(root, "tools", name, "tool"), []byte("staging bytes"), 0o666)
		lock.SetPin(name, "1.0", map[string]any{target: map[string]any{"url": "https://example.invalid/tool.zip", "sha256": strings.Repeat("a", 64)}})
		entries.Set(name, name)
	}
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	if err := recordTools(root, lock.Path, target, entries); err != nil {
		t.Fatal(err)
	}
	facts, err := NewFacts(filepath.Join(root, "tools/facts.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.RecordState("venv", "selection", []string{"web"}, "", ""); err != nil {
		t.Fatal(err)
	}
	return root
}

func bpRehash(root string) error {
	_, err := PayloadMain(&bytes.Buffer{}, "rehash", root)
	return err
}

func TestPayloadRehashRecordsAllChangedToolsAndPreservesIdentity(t *testing.T) {
	root := bpDigestPayload(t, t.TempDir())
	path := filepath.Join(root, "tools/facts.json")
	before := bpReadJSON(t, path)["packages"].(map[string]any)
	writeFile(t, filepath.Join(root, "tools/python/tool"), []byte("final python bytes"), 0o666)
	writeFile(t, filepath.Join(root, "tools/uv/tool"), []byte("final uv bytes"), 0o666)
	var out bytes.Buffer
	if _, err := PayloadMain(&out, "rehash", root); err != nil || out.String() != "rehashed 2 payload tools\n" {
		t.Fatalf("rehash %q %v", out.String(), err)
	}
	after := bpReadJSON(t, path)["packages"].(map[string]any)
	for _, name := range []string{"python", "uv"} {
		expected := map[string]any{}
		for k, v := range before[name].(map[string]any) {
			expected[k] = v
		}
		digest, err := TreeDigest(filepath.Join(root, "tools", expected["entry"].(string)))
		if err != nil {
			t.Fatal(err)
		}
		expected["digest"] = digest
		if !reflect.DeepEqual(after[name], expected) || digest == before[name].(map[string]any)["digest"] {
			t.Fatalf("%s: %v", name, after[name])
		}
	}
	if !reflect.DeepEqual(after["venv"], before["venv"]) {
		t.Fatal("venv state changed")
	}
	saved := readFile(t, path)
	if err := bpRehash(root); err != nil || !bytes.Equal(readFile(t, path), saved) {
		t.Fatalf("second rehash %v", err)
	}
}

func TestPayloadPackagedMetadataIsSharedButMutableWritesStayPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix modes")
	}
	tmp := t.TempDir()
	root := bpDigestPayload(t, tmp)
	path := filepath.Join(root, "tools/facts.json")
	if bpMode(t, path) != 0o600 {
		t.Fatalf("mode %o", bpMode(t, path))
	}
	target, _ := CurrentTarget()
	entries := NewOrderedObject()
	entries.Set("python", "python")
	entries.Set("uv", "uv")
	if err := recordTools(root, filepath.Join(tmp, "lock.json"), target, entries); err != nil || bpMode(t, path) != 0o644 {
		t.Fatalf("record %v %o", err, bpMode(t, path))
	}
	facts, _ := NewFacts(path, false)
	if err := facts.RecordState("venv", "selection", []string{"web"}, "", ""); err != nil || bpMode(t, path) != 0o600 {
		t.Fatalf("state %v %o", err, bpMode(t, path))
	}
	if count, err := rehashTools(root); err != nil || count != 2 || bpMode(t, path) != 0o644 {
		t.Fatalf("rehash %d %v %o", count, err, bpMode(t, path))
	}
	lock := filepath.Join(root, "venv/.lock")
	writeFile(t, lock, nil, 0o666)
	if err := relativizeLinks(root); err != nil || pathExists(lock) {
		t.Fatalf("lock kept %v", err)
	}
	private := filepath.Join(tmp, "private/facts.json")
	privateFacts, _ := NewFacts(private, false)
	if err := privateFacts.RecordState("venv", "private", []string{}, "", ""); err != nil || bpMode(t, private) != 0o600 {
		t.Fatalf("private %v", err)
	}
}

func TestPayloadPackagingNeverChangesExternalMetadataOrBuildLocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix links")
	}
	tmp := t.TempDir()
	root := bpDigestPayload(t, tmp)
	outside := filepath.Join(tmp, "external")
	private := filepath.Join(outside, "facts.json")
	privateFacts, _ := NewFacts(private, false)
	if err := privateFacts.RecordState("venv", "private", []string{}, "", ""); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, private)
	facts := filepath.Join(root, "tools/facts.json")
	os.Remove(facts)
	os.Symlink(private, facts)
	target, _ := CurrentTarget()
	uvOnly := NewOrderedObject()
	uvOnly.Set("uv", "uv")
	for _, operation := range []func() error{
		func() error { return recordTools(root, filepath.Join(tmp, "lock.json"), target, uvOnly) },
		func() error { _, err := rehashTools(root); return err },
	} {
		if err := operation(); err == nil || !strings.Contains(err.Error(), "symlinked") {
			t.Fatalf("symlinked facts accepted: %v", err)
		}
		if !bytes.Equal(readFile(t, private), before) || bpMode(t, private) != 0o600 {
			t.Fatal("external facts changed")
		}
	}
	lock := filepath.Join(outside, ".lock")
	writeFile(t, lock, []byte("external lock"), 0o600)
	os.Symlink(outside, filepath.Join(root, "venv"))
	if err := relativizeLinks(root); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("escaping venv accepted: %v", err)
	}
	if string(readFile(t, lock)) != "external lock" || bpMode(t, lock) != 0o600 {
		t.Fatal("external lock changed")
	}
}

func TestPayloadSealingStripsBOMAndAbsoluteBuildPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "payload")
	python := filepath.Join(root, "tools/pythön/python")
	writeFile(t, python, []byte("interpreter"), 0o666)
	runtimeDir := filepath.Join(root, "pm-runtime")
	for _, dir := range []string{"Lib/site-packages", "bin", "Scripts"} {
		os.MkdirAll(filepath.Join(runtimeDir, dir), 0o777)
	}
	cfg := filepath.Join(runtimeDir, "pyvenv.cfg")
	writeFile(t, cfg, []byte("\xef\xbb\xbfhome = /private/builder\nexecutable = /private/python\nversion = 3.14\n"), 0o666)
	if err := sealPMRuntime(context.Background(), root, python); err != nil {
		t.Fatal(err)
	}
	data := readFile(t, cfg)
	if bytes.HasPrefix(data, []byte("\xef\xbb\xbf")) {
		t.Fatal("BOM kept")
	}
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), "\n")
	if want := []string{"home = " + filepath.Join("..", "tools", "pythön"), "version = 3.14"}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("cfg %q", lines)
	}
	if got := bpReadJSON(t, filepath.Join(runtimeDir, "pm-runtime.json"))["python"]; got != "../tools/pythön/python" {
		t.Fatalf("marker python %v", got)
	}
}

func TestPayloadInvalidToolEvidenceNeverPartiallyRewritesFacts(t *testing.T) {
	tmp := t.TempDir()
	root := bpDigestPayload(t, tmp)
	path := filepath.Join(root, "tools/facts.json")
	before := bpReadJSON(t, path)
	writeFile(t, filepath.Join(root, "tools/python/tool"), []byte("final python bytes"), 0o666)
	outside := filepath.Join(tmp, "outside")
	writeFile(t, filepath.Join(outside, "kept"), []byte("foreign"), 0o666)
	for _, entry := range []string{"missing", "../outside", outside} {
		invalid := bpReadJSON(t, path)
		invalid["packages"] = before["packages"]
		raw, _ := json.Marshal(invalid)
		var copied map[string]any
		json.Unmarshal(raw, &copied)
		copied["packages"].(map[string]any)["uv"].(map[string]any)["entry"] = entry
		text, _ := json.Marshal(copied)
		writeFile(t, path, text, 0o666)
		saved := readFile(t, path)
		if err := bpRehash(root); err == nil {
			t.Fatalf("entry %s accepted", pyrepr.StrRepr(entry))
		}
		if !bytes.Equal(readFile(t, path), saved) {
			t.Fatalf("entry %s partially rewrote facts", entry)
		}
	}
	for _, raw := range []string{"not JSON", `{"schema":1,"packages":{}}`} {
		writeFile(t, path, []byte(raw), 0o666)
		if err := bpRehash(root); err == nil || string(readFile(t, path)) != raw {
			t.Fatalf("raw %q: %v", raw, err)
		}
	}
	os.Remove(path)
	if err := bpRehash(root); err == nil || pathExists(path) {
		t.Fatalf("missing facts: %v", err)
	}
}
