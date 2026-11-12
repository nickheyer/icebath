package builtin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Upstream EGM checkout files the cases read beyond the entries the payload ships
var egmPackRepo = filepath.Join("testdata", "elephant-goldfish-model")

// Checkout path of a slash relative EGM entry, the payload copy for every entry the package ships
func egmRepoPath(rel string) string {
	switch first, _, _ := strings.Cut(rel, "/"); first {
	case "skills", "agents", "resources", "docs", "assets", ".codex-plugin", "plugin.json", "README.md", "LICENSE", "TESTING.md":
		return filepath.Join("payload", "elephant-goldfish-model", filepath.FromSlash(rel))
	}
	return filepath.Join(egmPackRepo, filepath.FromSlash(rel))
}

// Calls build from the materialized pack_plugin.py and prints its paths or the raised exception as JSON
const egmPackDriver = `import json, runpy, sys, zipfile
from pathlib import Path
script, source, target, output, claim, outside = sys.argv[1:7]
build = runpy.run_path(script)["build"]
if claim:
    archive = Path(output) / f"egm-{target}.zip"
    original = zipfile.ZipFile
    def claim_archive(*args, **kwargs):
        if claim != "file":
            archive.symlink_to(Path(outside))
        else:
            archive.write_bytes(b"another build owns this file")
        return original(*args, **kwargs)
    zipfile.ZipFile = claim_archive
try:
    folder, bundle = build(Path(source), target, Path(output))
    print(json.dumps({"folder": str(folder), "archive": str(bundle)}))
except Exception as exc:
    print(json.dumps({"type": type(exc).__name__, "error": str(exc)}))
`

var (
	egmPackOnce   sync.Once
	egmPackPython string
	egmPackScript string
	egmPackErr    error
)

// Outcome of one build call, paths on success or the exception class and text
type egmPackResult struct {
	Folder, Archive, Type, Error string
}

// Managed python of a run wide home and the materialized pack_plugin.py shared by every packaging test
func egmPackTools(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", filepath.Join(builtinTestCache, "toolchain-home"))
	defer t.Setenv("ICEBATH_HOME", t.TempDir())
	egmPackOnce.Do(func() {
		var found bool
		_, egmPackPython, found, egmPackErr = pm.Toolchain(context.Background(), true, false)
		if egmPackErr == nil && !found {
			egmPackErr = errors.New("managed python not found")
		}
		if egmPackErr != nil {
			return
		}
		var tree *Tree
		if tree, egmPackErr = materializeAt(builtinTestCache); egmPackErr == nil {
			egmPackScript = tree.Payload("elephant-goldfish-model/scripts/pack_plugin.py")
		}
	})
	if egmPackErr != nil {
		t.Fatal(egmPackErr)
	}
	return egmPackPython, egmPackScript
}

// Runs build through the managed python, claim names the zipfile.ZipFile race to simulate
func egmPackBuild(t *testing.T, source, target, output, claim, outside string) egmPackResult {
	t.Helper()
	python, script := egmPackTools(t)
	out, err := exec.Command(python, "-I", "-c", egmPackDriver, script, source, target, output, claim, outside).CombinedOutput()
	var r egmPackResult
	if err != nil || json.Unmarshal(out, &r) != nil {
		t.Fatalf("pack driver %v\n%s", err, out)
	}
	return r
}

// Successful build paths
func egmPackOK(t *testing.T, source, target, output string) (string, string) {
	t.Helper()
	r := egmPackBuild(t, source, target, output, "", "")
	if r.Type != "" {
		t.Fatalf("build %s %s: %s: %s", target, output, r.Type, r.Error)
	}
	return r.Folder, r.Archive
}

// Asserts build raised the exception class with text containing want
func egmPackRaises(t *testing.T, source, target, output, class, want string) egmPackResult {
	t.Helper()
	r := egmPackBuild(t, source, target, output, "", "")
	if r.Type != class || !strings.Contains(r.Error, want) {
		t.Fatalf("build %s want %s containing %q, got %+v", target, class, want, r)
	}
	return r
}

// Source tree copied from the EGM checkout the way the upstream fixture builds it
func egmPackSource(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "source")
	for _, name := range []string{"plugin.json", "LICENSE", "README.md", "TESTING.md", "CONTRIBUTING.md", "AI_POLICY.md", ".github/PULL_REQUEST_TEMPLATE.md"} {
		data, err := os.ReadFile(egmRepoPath(name))
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755)
		os.WriteFile(filepath.Join(root, name), data, 0o644)
	}
	for _, name := range []string{"skills", "agents", "resources", "docs", "assets", ".claude-plugin", ".codex-plugin"} {
		if err := os.CopyFS(filepath.Join(root, name), os.DirFS(egmRepoPath(name))); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// Rewrites a JSON manifest after edit changes its decoded ordered form
func egmEditJSON(t *testing.T, p string, ascii bool, edit func(o *pyjson.Object)) []byte {
	t.Helper()
	v, err := pyjson.Loads(string(must(os.ReadFile(p))))
	if err != nil {
		t.Fatal(err)
	}
	edit(v.(*pyjson.Object))
	text, err := pyjson.Dumps(v, pyjson.EnsureASCII(ascii))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte(text), 0o644)
	return []byte(text)
}

// Nested object under a chain of keys
func egmObj(o *pyjson.Object, keys ...string) *pyjson.Object {
	for _, k := range keys {
		v, _ := o.Get(k)
		o = v.(*pyjson.Object)
	}
	return o
}

// Entry bytes of every file in a zip by name
func egmZip(t *testing.T, archive string) (map[string][]byte, []*zip.File) {
	t.Helper()
	r, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	out := map[string][]byte{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("testzip %s: %v", f.Name, err)
		}
		out[f.Name] = data
	}
	return out, r.File
}

func gonePath(p string) bool {
	_, err := os.Lstat(p)
	return errors.Is(err, fs.ErrNotExist)
}

func TestEGMPackRuntimeBundleIsCompleteAndReproducible(t *testing.T) {
	for _, target := range []string{"claude", "openai"} {
		t.Run(target, func(t *testing.T) {
			source, tmp := egmPackSource(t), t.TempDir()
			for _, name := range []string{"site/font.woff2", "tests/secret.txt", ".github/workflows/a.yml", ".agents/plugins/marketplace.json", "hooks/run.sh", ".claude-plugin/hooks/run.sh", "assets/private.txt", "docs/experimental-host.md"} {
				os.MkdirAll(filepath.Dir(filepath.Join(source, name)), 0o755)
				os.WriteFile(filepath.Join(source, name), []byte("must not ship"), 0o644)
			}
			first, archive := egmPackOK(t, source, target, filepath.Join(tmp, "first"))
			_, repeated := egmPackOK(t, source, target, filepath.Join(tmp, "second"))
			if !bytes.Equal(must(os.ReadFile(archive)), must(os.ReadFile(repeated))) {
				t.Fatal("archives differ between builds")
			}
			entries, files := egmZip(t, archive)
			skills := 0
			for name := range entries {
				if strings.HasSuffix(name, "/SKILL.md") {
					skills++
				}
			}
			if skills != 7 {
				t.Fatalf("%d SKILL.md entries", skills)
			}
			matches, _ := filepath.Glob(filepath.Join(source, "skills", "*", "SKILL.md"))
			for _, skill := range matches {
				prefix := "skills/" + filepath.Base(filepath.Dir(skill))
				meta, err := config.FastSafeLoad(entries[prefix+"/agents/openai.yaml"])
				if err != nil {
					t.Fatal(err)
				}
				iface := meta.(map[string]any)["interface"].(map[string]any)
				short, _ := iface["short_description"].(string)
				if iface["display_name"] == "" || iface["display_name"] == nil || len([]rune(short)) < 25 || len([]rune(short)) > 64 {
					t.Fatalf("%s interface %v", prefix, iface)
				}
				for _, field := range []string{"icon_small", "icon_large"} {
					icon := iface[field].(string)
					if path.IsAbs(icon) || filepath.IsAbs(icon) || strings.Contains("/"+icon+"/", "/../") {
						t.Fatalf("%s %s %s", prefix, field, icon)
					}
					data := entries[path.Clean(prefix+"/"+icon)]
					if !bytes.Equal(data, must(os.ReadFile(filepath.Join(filepath.Dir(skill), icon)))) {
						t.Fatalf("%s %s bytes differ", prefix, field)
					}
					var start xml.StartElement
					for d := xml.NewDecoder(bytes.NewReader(data)); ; {
						tok, err := d.Token()
						if err != nil {
							t.Fatalf("%s svg %v", prefix, err)
						}
						if se, ok := tok.(xml.StartElement); ok {
							start = se
							break
						}
					}
					if start.Name.Space != "http://www.w3.org/2000/svg" || start.Name.Local != "svg" {
						t.Fatalf("%s root %v", prefix, start.Name)
					}
				}
			}
			for _, name := range []string{"skills/egm-bootstrap/references/worker-return.schema.json", "skills/egm-bootstrap/references/templates.md", "resources/document_standards/feature-spec-standard.md", "agents/design_critic.md", "docs/codex.md", "docs/claude.md", "assets/icon.svg"} {
				if _, ok := entries[name]; !ok {
					t.Fatalf("missing %s", name)
				}
			}
			for _, m := range regexp.MustCompile(`<img\b[^>]*\bsrc="([^"]+)"`).FindAllStringSubmatch(string(entries["README.md"]), -1) {
				if _, ok := entries[m[1]]; !ok {
					t.Fatalf("Unbundled README image: %s", m[1])
				}
			}
			for name := range entries {
				bad := name == "CONTRIBUTING.md" || name == "AI_POLICY.md" || name == "TESTING.md" || name == ".github/PULL_REQUEST_TEMPLATE.md" ||
					strings.HasPrefix(name, "docs/designs/") || strings.HasPrefix(name, "site/") || strings.HasPrefix(name, "tests/") ||
					strings.HasPrefix(name, ".github/workflows/") || strings.HasPrefix(name, ".agents/") || strings.Contains(name, "hooks/") ||
					strings.HasSuffix(name, "test_scenarios.yaml") || strings.HasSuffix(name, "/TEST.md") || strings.HasSuffix(name, "marketplace.json") ||
					name == "assets/private.txt" || name == "docs/experimental-host.md" || name == "docs/directory.md"
				if bad {
					t.Fatalf("shipped %s", name)
				}
			}
			for name, claude := range map[string]bool{".claude-plugin/plugin.json": true, ".claude-plugin/icon.png": true, ".claude-plugin/agents/style.md": true, ".codex-plugin/plugin.json": false} {
				if _, ok := entries[name]; ok != (claude == (target == "claude")) {
					t.Fatalf("%s present %v for %s", name, ok, target)
				}
			}
			epoch := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
			for _, f := range files {
				if path.IsAbs(f.Name) || strings.Contains("/"+f.Name+"/", "/../") || !f.Modified.Equal(epoch) || f.ExternalAttrs>>16 != 0o100644 {
					t.Fatalf("entry %s %v %o", f.Name, f.Modified, f.ExternalAttrs>>16)
				}
				if !bytes.Equal(entries[f.Name], must(os.ReadFile(filepath.Join(first, filepath.FromSlash(f.Name))))) {
					t.Fatalf("%s differs from the runtime folder", f.Name)
				}
			}
			checkEGMLinks(t, first)
		})
	}
}

func TestEGMPackBundlesReadmeImages(t *testing.T) {
	for _, target := range []string{"claude", "openai"} {
		source := egmPackSource(t)
		imageDir := filepath.Join(source, "docs", "assets")
		os.MkdirAll(imageDir, 0o755)
		image := filepath.Join(imageDir, "readme-header.svg")
		os.WriteFile(image, []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), 0o644)
		os.WriteFile(filepath.Join(imageDir, "private.txt"), []byte("must not ship"), 0o644)
		readme := filepath.Join(source, "README.md")
		os.WriteFile(readme, append(must(os.ReadFile(readme)), []byte("\n![Header](docs/assets/readme-header.svg)\n")...), 0o644)
		_, archive := egmPackOK(t, source, target, filepath.Join(t.TempDir(), "output"))
		entries, _ := egmZip(t, archive)
		if !bytes.Equal(entries["docs/assets/readme-header.svg"], must(os.ReadFile(image))) {
			t.Fatalf("%s header image", target)
		}
		if _, ok := entries["docs/assets/private.txt"]; ok {
			t.Fatalf("%s shipped private.txt", target)
		}
	}
}

func TestEGMPackRejectsSymlinkedRuntimePaths(t *testing.T) {
	for _, directory := range []bool{false, true} {
		source, tmp := egmPackSource(t), t.TempDir()
		victim := filepath.Join(source, "assets", "icon.svg")
		if directory {
			victim = filepath.Join(source, "agents")
		}
		outside := filepath.Join(tmp, "outside")
		if err := os.Rename(victim, outside); err != nil {
			t.Fatal(err)
		}
		os.Symlink(outside, victim)
		egmPackRaises(t, source, "openai", filepath.Join(tmp, "output"), "ValueError", "Symlink")
	}
}

func TestEGMPackReadsManifestsAsUTF8(t *testing.T) {
	for _, target := range []string{"claude", "openai"} {
		source := egmPackSource(t)
		for _, name := range []string{"plugin.json", ".claude-plugin/plugin.json", ".codex-plugin/plugin.json"} {
			egmEditJSON(t, filepath.Join(source, name), false, func(o *pyjson.Object) { o.Set("description", "\u0401lephant \u2014 goldfish") })
		}
		_, archive := egmPackOK(t, source, target, filepath.Join(t.TempDir(), "output"))
		if entries, _ := egmZip(t, archive); !strings.Contains(string(entries["plugin.json"]), "\u0401lephant") {
			t.Fatalf("%s plugin.json %s", target, entries["plugin.json"])
		}
	}
}

func TestEGMPackRejectsEscapingOrMissingManifestAssets(t *testing.T) {
	for _, ref := range []string{"../escape.svg", "/tmp/icon.svg", "./assets/missing.svg"} {
		source := egmPackSource(t)
		egmEditJSON(t, filepath.Join(source, "plugin.json"), true, func(o *pyjson.Object) {
			egmObj(o, "extensions", "com.openai", "interface").Set("logo", ref)
		})
		egmPackRaises(t, source, "openai", filepath.Join(t.TempDir(), "output"), "ValueError", "Unbundled")
	}
}

func TestEGMPackClaudeAgentReferenceForms(t *testing.T) {
	for _, agents := range []any{"./.claude-plugin/agents/style.md", []any{"./.claude-plugin/agents/style.md"}} {
		source := egmPackSource(t)
		manifest := filepath.Join(source, ".claude-plugin", "plugin.json")
		written := egmEditJSON(t, manifest, true, func(o *pyjson.Object) { o.Set("agents", agents) })
		folder, archive := egmPackOK(t, source, "claude", filepath.Join(t.TempDir(), "output"))
		ref := ".claude-plugin/agents/style.md"
		want := must(os.ReadFile(filepath.Join(source, ref)))
		entries, _ := egmZip(t, archive)
		if !bytes.Equal(must(os.ReadFile(filepath.Join(folder, ref))), want) || !bytes.Equal(must(os.ReadFile(filepath.Join(folder, ".claude-plugin", "plugin.json"))), written) ||
			!bytes.Equal(entries[ref], want) || !bytes.Equal(entries[".claude-plugin/plugin.json"], written) {
			t.Fatalf("agents %v not carried", agents)
		}
	}
}

func TestEGMPackRejectsInvalidManifestAgents(t *testing.T) {
	invalid := []struct {
		value any
		repr  string
	}{{nil, "None"}, {int64(5), "5"}, {true, "True"}, {pyjson.NewObject(), "{}"}, {pyjson.ObjectOf("./agents/style.md", nil), "{'./agents/style.md': None}"}, {1.5, "1.5"}}
	for _, host := range [][2]string{{"claude", ".claude-plugin"}, {"openai", ".codex-plugin"}} {
		for _, a := range invalid {
			source, output := egmPackSource(t), filepath.Join(t.TempDir(), "output")
			egmEditJSON(t, filepath.Join(source, host[1], "plugin.json"), true, func(o *pyjson.Object) { o.Set("agents", a.value) })
			if r := egmPackRaises(t, source, host[0], output, "ValueError", ""); r.Error != "Invalid manifest agents: "+a.repr || !gonePath(output) {
				t.Fatalf("%s %s: %+v", host[0], a.repr, r)
			}
		}
	}
}

func TestEGMPackRejectsEscapingOrMissingScalarAgentReferences(t *testing.T) {
	for _, ref := range []string{"./.claude-plugin/agents/missing.md", "../escape.md", "/tmp/agent.md"} {
		source, output := egmPackSource(t), filepath.Join(t.TempDir(), "output")
		egmEditJSON(t, filepath.Join(source, ".claude-plugin", "plugin.json"), true, func(o *pyjson.Object) { o.Set("agents", ref) })
		if r := egmPackRaises(t, source, "claude", output, "ValueError", ""); r.Error != "Unbundled manifest reference: "+ref || !gonePath(output) {
			t.Fatalf("%s: %+v", ref, r)
		}
	}
}

func TestEGMPackRejectsMissingAndUnbundledInterfaceIcons(t *testing.T) {
	for _, key := range []string{"composerIcon", "logo"} {
		source := egmPackSource(t)
		egmEditJSON(t, filepath.Join(source, "plugin.json"), true, func(o *pyjson.Object) { egmObj(o, "extensions", "com.openai", "interface").Delete(key) })
		egmPackRaises(t, source, "openai", filepath.Join(t.TempDir(), "output"), "ValueError", "Missing OpenAI interface reference: "+key)
		source = egmPackSource(t)
		egmEditJSON(t, filepath.Join(source, ".codex-plugin", "plugin.json"), true, func(o *pyjson.Object) { egmObj(o, "interface").Set(key, "./assets/missing.svg") })
		egmPackRaises(t, source, "openai", filepath.Join(t.TempDir(), "output"), "ValueError", "Unbundled manifest reference")
	}
}

func TestEGMPackRefusesHooksOverwritesAndUnbundledServices(t *testing.T) {
	source, tmp := egmPackSource(t), t.TempDir()
	output := filepath.Join(tmp, "output")
	egmPackOK(t, source, "claude", output)
	egmPackRaises(t, source, "claude", output, "ValueError", "already exists")
	egmEditJSON(t, filepath.Join(source, ".claude-plugin", "plugin.json"), true, func(o *pyjson.Object) {
		o.Set("hooks", pyjson.ObjectOf("PreToolUse", []any{}))
	})
	egmPackRaises(t, source, "claude", filepath.Join(tmp, "other"), "ValueError", "hooks")
	source = egmPackSource(t)
	egmEditJSON(t, filepath.Join(source, "plugin.json"), true, func(o *pyjson.Object) { egmObj(o, "extensions", "com.openai").Set("hooks", "./hooks/hooks.json") })
	egmPackRaises(t, source, "openai", filepath.Join(t.TempDir(), "output"), "ValueError", "hooks")
	for _, field := range []string{"apps", "mcpServers"} {
		source = egmPackSource(t)
		egmEditJSON(t, filepath.Join(source, "plugin.json"), true, func(o *pyjson.Object) { egmObj(o, "extensions", "com.openai").Set(field, "./missing.json") })
		egmPackRaises(t, source, "openai", filepath.Join(t.TempDir(), "output"), "ValueError", "dependencies")
	}
}

func TestEGMPackIncompleteSourceFailsBeforeOutput(t *testing.T) {
	missing := []string{"skills", "skills/egm-bootstrap/SKILL.md", "skills/egm-brainstorm/SKILL.md", "skills/egm-draft/SKILL.md", "skills/egm-drift-report/SKILL.md",
		"skills/egm-resolve/SKILL.md", "skills/egm-review/SKILL.md", "skills/egm-review-loop/SKILL.md", "skills/egm-review/agents/openai.yaml", "skills/egm-review/assets/icon.svg",
		"skills/egm-bootstrap/references/worker-return.schema.json", "skills/egm-bootstrap/references/templates.md", "skills/egm-draft/references/design_doc.md",
		"agents/design_critic.md", "resources/execution_instructions.md", "resources/document_standards/zoom-out-standard.md", "docs/harnesses.md"}
	for _, target := range []string{"claude", "openai"} {
		for _, name := range missing {
			source, output := egmPackSource(t), filepath.Join(t.TempDir(), "output")
			if err := os.RemoveAll(filepath.Join(source, name)); err != nil {
				t.Fatal(err)
			}
			egmPackRaises(t, source, target, output, "ValueError", "Missing required file")
			if !gonePath(output) {
				t.Fatalf("%s %s left output", target, name)
			}
		}
	}
}

func TestEGMPackRejectsDanglingOutputSymlinks(t *testing.T) {
	source := egmPackSource(t)
	for _, target := range []string{"claude", "openai"} {
		for _, suffix := range []string{"", ".zip"} {
			tmp := t.TempDir()
			output, outside := filepath.Join(tmp, "output"), filepath.Join(tmp, "outside")
			os.Mkdir(output, 0o755)
			link := filepath.Join(output, "egm-"+target+suffix)
			os.Symlink(outside, link)
			egmPackRaises(t, source, target, output, "ValueError", "already exists")
			fi, err := os.Lstat(link)
			entries, _ := os.ReadDir(output)
			if err != nil || fi.Mode()&fs.ModeSymlink == 0 || !gonePath(outside) || len(entries) != 1 {
				t.Fatalf("%s%s link %v entries %v", target, suffix, err, entries)
			}
		}
	}
}

func TestEGMPackArchiveClaimedAfterPreflightIsNotOverwritten(t *testing.T) {
	source := egmPackSource(t)
	for _, target := range []string{"claude", "openai"} {
		for _, claim := range []string{"file", "dangling-symlink", "symlink"} {
			tmp := t.TempDir()
			output, outside := filepath.Join(tmp, "output"), filepath.Join(tmp, "outside.zip")
			archive := filepath.Join(output, "egm-"+target+".zip")
			if claim == "symlink" {
				os.WriteFile(outside, []byte("outside file must not change"), 0o644)
			}
			if r := egmPackBuild(t, source, target, output, claim, outside); r.Type != "FileExistsError" {
				t.Fatalf("%s %s: %+v", target, claim, r)
			}
			fi, err := os.Lstat(archive)
			switch {
			case err != nil:
				t.Fatalf("%s %s archive %v", target, claim, err)
			case claim == "file" && string(must(os.ReadFile(archive))) != "another build owns this file":
				t.Fatalf("%s claimed file overwritten", target)
			case claim != "file" && fi.Mode()&fs.ModeSymlink == 0:
				t.Fatalf("%s %s archive is no symlink", target, claim)
			case claim == "symlink" && string(must(os.ReadFile(outside))) != "outside file must not change":
				t.Fatalf("%s outside changed", target)
			case claim == "dangling-symlink" && !gonePath(outside):
				t.Fatalf("%s outside created", target)
			}
			if staged, _ := filepath.Glob(filepath.Join(output, ".egm-*")); len(staged) != 0 {
				t.Fatalf("%s %s staging left %v", target, claim, staged)
			}
		}
	}
}

func TestEGMPackPublishedClaudeRuntimeIsCurrent(t *testing.T) {
	generated, _ := egmPackOK(t, egmPackSource(t), "claude", t.TempDir())
	contents := func(folder string) map[string]string {
		out := map[string]string{}
		filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				rel, _ := filepath.Rel(folder, p)
				out[filepath.ToSlash(rel)] = string(must(os.ReadFile(p)))
			}
			return nil
		})
		return out
	}
	published, got := contents(filepath.Join(egmPackRepo, "distributions", "claude")), contents(generated)
	if len(published) != len(got) {
		t.Fatalf("Published Claude runtime differs from generated output: %d published files, %d generated", len(published), len(got))
	}
	for name, body := range published {
		if got[name] != body {
			t.Fatal(fmt.Sprintf("Published Claude runtime differs from generated output at %s", name))
		}
	}
}
