package builtin

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tarzst"
)

// Scripts path the rendered skill carries in place of its scripts path token
const renderScriptsPath = "/host/impeccable/skills/impeccable/scripts"

// Text of every file the icebath render writes for the current payload, keyed by its path below the skill directory
func renderedSkill(t *testing.T) map[string]string {
	t.Helper()
	files, err := renderImpeccableSkill(renderScriptsPath)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for name, f := range files {
		out[name] = string(f.Data)
	}
	return out
}

// Render of the skill tree under root/skill, which stands in for the embedded payload while the test runs
func renderSourceSkill(t *testing.T, root string) map[string]string {
	t.Helper()
	source, err := tarzst.Collect(root, false)
	if err != nil {
		t.Fatal(err)
	}
	packed := []tarzst.File{tarzst.NewFile("impeccable/plugin/.claude-plugin/plugin.json", []byte(`{"version": "1.2.3-test"}`), 0o644)}
	for _, f := range source {
		packed = append(packed, tarzst.NewFile("impeccable/"+f.Path, f.Data, f.Mode))
	}
	archive, err := tarzst.Encode(packed)
	if err != nil {
		t.Fatal(err)
	}
	keptFS, keptManifest := payloadFS, payloadManifest
	t.Cleanup(func() { payloadFS, payloadManifest = keptFS, keptManifest })
	payloadFS, payloadManifest = tarzst.NewFS(archive, "payload"), tarzst.Manifest(packed)
	return renderedSkill(t)
}

// Sorted paths of the rendered files below a directory prefix, the prefix cut
func renderedUnder(files map[string]string, prefix string) []string {
	var names []string
	for name := range files {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			names = append(names, rest)
		}
	}
	sort.Strings(names)
	return names
}

var (
	ruleMarkerOpenRe  = regexp.MustCompile(`<!--\s*rule:`)
	placeholderOpenRe = regexp.MustCompile(`\{\{[a-z_]+\}\}`)
)

// Scripts of the rendered embedded skill by their path below scripts
func sourceScripts(t *testing.T) map[string]string {
	t.Helper()
	scripts := map[string]string{}
	for name, content := range renderedSkill(t) {
		if rest, ok := strings.CutPrefix(name, "scripts/"); ok {
			scripts[rest] = content
		}
	}
	return scripts
}

func TestSkillScriptsPayloadShipsTheLauncherVersionPageJSAndCommandMetadata(t *testing.T) {
	scripts := sourceScripts(t)
	for _, expected := range []string{"impeccable", "impeccable.cmd", "VERSION", "command-metadata.json",
		"live-browser.js", "live-browser-dom.js", "live-browser-session.js", "modern-screenshot.umd.js"} {
		if _, ok := scripts[expected]; !ok {
			t.Errorf("scripts payload lacks %s", expected)
		}
	}
}

func TestSkillScriptsPayloadShipsTheWindowsLauncherWithCRLFLineEndings(t *testing.T) {
	cmd, ok := sourceScripts(t)["impeccable.cmd"]
	if !ok || !strings.Contains(cmd, "\r\n") || regexp.MustCompile(`(^|[^\r])\n`).MatchString(cmd) {
		t.Fatalf("impeccable.cmd present %v holds a bare LF or no CRLF", ok)
	}
}

func TestSkillScriptsPayloadShipsNoEngineEntryPointsAndNoBundledDetector(t *testing.T) {
	allowedNodeScripts := []string{"build-phase.mjs", "comp-diff.mjs", "comp-spec.mjs", "font-match.mjs", "lib/font-fingerprint.mjs",
		"lib/font-index.mjs", "lib/hero-checks.mjs", "lib/image-metrics.mjs", "lib/png.mjs", "lib/raster.mjs"}
	var stray []string
	for name := range sourceScripts(t) {
		if (strings.HasSuffix(name, ".mjs") || strings.HasPrefix(name, "detector/") || strings.HasPrefix(name, "lib/")) && !slices.Contains(allowedNodeScripts, name) {
			stray = append(stray, name)
		}
	}
	if len(stray) != 0 {
		t.Fatalf("stray scripts %v", stray)
	}
}

func TestSkillScriptsPayloadNeverReadsPlatformBinariesAsSource(t *testing.T) {
	for name := range sourceScripts(t) {
		if strings.HasPrefix(name, "bin/") {
			t.Errorf("platform binary %s rendered as a script", name)
		}
	}
	root := t.TempDir()
	writeAt(t, root, "skill/SKILL.src.md", "---\nname: impeccable\n---\n\nBody.")
	writeAt(t, root, "skill/scripts/bin/linux-x64/impeccable", "binary")
	writeAt(t, root, "skill/scripts/config.json", "{}")
	writeAt(t, root, "skill/scripts/data/bin/kept.json", "{}")
	if names := renderedUnder(renderSourceSkill(t, root), "scripts/"); !slices.Equal(names, []string{"data/bin/kept.json"}) {
		t.Fatalf("scripts rendered %v", names)
	}
}

// Rendered degraded fallback of one agent role of the embedded skill
func readDegraded(t *testing.T, role string) string {
	t.Helper()
	return renderedSkill(t)["reference/degraded/"+role+".md"]
}

var degradedRoles = []string{"asset-producer.md", "documenter.md", "finish-reviewer.md", "manual-edit-applier.md"}

func TestDegradedModeFallbackReferenceGenerationABuildEmitsReferenceDegradedRoleMdForEveryAgentPrefixStripped(t *testing.T) {
	if files := renderedUnder(renderedSkill(t), "reference/degraded/"); !slices.Equal(files, degradedRoles) {
		t.Fatalf("degraded files %v", files)
	}
}

func TestDegradedModeFallbackReferenceGenerationFinishReviewerFallbackOpensWithThePreambleAndCarriesADistinctiveBodyPhrase(t *testing.T) {
	content := readDegraded(t, "finish-reviewer")
	if !strings.HasPrefix(content, "<!-- Generated from skill/agents/ at build time. Do not edit; edit the agent definition. -->") {
		t.Fatalf("fallback opens with %q", content[:min(len(content), 120)])
	}
	wantContains(t, content, "This harness has no subagent capability, so you are running this role inline.", "material_fixes")
}

func TestDegradedModeFallbackReferenceGenerationTheDisclosureStaysOutOfTheOutputContract(t *testing.T) {
	wantContains(t, readDegraded(t, "manual-edit-applier"), "outside the output contract")
}

func TestDegradedModeFallbackReferenceGenerationGeneratedFallbacksPassThroughProviderBlockCompilation(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "skill/SKILL.src.md", "---\nname: impeccable\ndescription: synthetic\n---\n\nSynthetic skill body.")
	writeAt(t, root, "skill/agents/impeccable-synthetic.md", "---\nname: impeccable-synthetic\n---\n\nShared body line.\n\n"+
		"<icebath>\nICEBATH_ONLY_MARKER for the icebath target.\n</icebath>\n\n<codex>\nCODEX_ONLY_MARKER for the codex target.\n</codex>\n\nMore shared body.")
	fallback := renderSourceSkill(t, root)["reference/degraded/synthetic.md"]
	wantContains(t, fallback, "ICEBATH_ONLY_MARKER", "More shared body.")
	wantLacks(t, fallback, "CODEX_ONLY_MARKER", "<icebath>")
	if !strings.HasPrefix(fallback, "<!-- Generated from skill/agents/") {
		t.Fatalf("fallback opens with %q", fallback[:min(len(fallback), 80)])
	}
}

func TestAgentBodiesResolvePlaceholdersOnEverySurfaceThatShipsThemNoEmittedAgentBodyCarriesAnUnresolvedPlaceholderOrARuleMarker(t *testing.T) {
	for name, content := range renderedSkill(t) {
		if strings.HasSuffix(name, ".md") && (placeholderOpenRe.MatchString(content) || ruleMarkerOpenRe.MatchString(content)) {
			t.Errorf("%s keeps a placeholder token or rule marker", name)
		}
	}
	root := t.TempDir()
	writeAt(t, root, "skill/SKILL.src.md", "---\nname: impeccable\ndescription: synthetic\n---\n\nSynthetic skill body.")
	writeAt(t, root, "skill/agents/impeccable-synthetic.md", "---\nname: impeccable-synthetic\ndescription: synthetic agent\n---\n\n"+
		"Run `{{scripts_path}}/impeccable embed-prompt` and ask {{model}}. See [x.md]({{reference_path}}/x.md). <!-- rule:synthetic-marker -->")
	fallback := renderSourceSkill(t, root)["reference/degraded/synthetic.md"]
	if strings.Contains(fallback, "{{") || ruleMarkerOpenRe.MatchString(fallback) {
		t.Errorf("the fallback keeps a brace token or rule marker\n%s", fallback)
	}
	wantContains(t, fallback, "Run `"+renderScriptsPath+"/impeccable embed-prompt` and ask the model. See [x.md](../x.md).")
}

func TestBatchLineEndingsToCrlfConvertsLFAndLeavesCRLFAlone(t *testing.T) {
	root := t.TempDir()
	writeAt(t, root, "skill/SKILL.src.md", "---\nname: impeccable\n---\n\nBody.")
	writeAt(t, root, "skill/scripts/impeccable.cmd", "a\nb\r\nc\n")
	writeAt(t, root, "skill/scripts/impeccable", "#!/bin/sh\nexit 0\n")
	files := renderSourceSkill(t, root)
	if got := files["scripts/impeccable.cmd"]; got != "a\r\nb\r\nc\r\n" {
		t.Fatalf("batch launcher %q", got)
	}
	if got := files["scripts/impeccable"]; got != "#!/bin/sh\nexit 0\n" {
		t.Fatalf("shell launcher %q", got)
	}
}
