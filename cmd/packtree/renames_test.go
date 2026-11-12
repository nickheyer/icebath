package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tarzst"
)

func TestRenameTextFollowsProductAndKeepsUpstreamNames(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Run `hermes skills install` then `hermes setup`.", "Run `icebath skills install` then `icebath setup`."},
		{`val = os.environ.get("HERMES_HOME", "")`, `val = os.environ.get("ICEBATH_HOME", "")`},
		{"tokens live under ~/.hermes/google_token.json", "tokens live under ~/.icebath/google_token.json"},
		{"from hermes_constants import get_hermes_home", "from icebath_constants import get_icebath_home"},
		{"from _hermes_home import display_hermes_home", "from _icebath_home import display_icebath_home"},
		{"orchestrate Hermes Agent.", "orchestrate Icebath Agent."},
		{`description: "Use, configure, theme, extend, and orchestrate Hermes Agent."`, `description: "Use, configure, theme, extend and orchestrate Icebath Agent."`},
		{"is outside Hermes' normal OAuth content work", "is outside Icebath's normal OAuth content work"},
		{"Attach Hermes's browser", "Attach Icebath's browser"},
		{`print('Hermes' + x)`, `print('Icebath' + x)`},
		{"see https://hermes-agent.nousresearch.com/docs/llms.txt for Hermes", "see https://hermes-agent.nousresearch.com/docs/llms.txt for Icebath"},
		{"docker pull nousresearch/hermes-agent:latest", "docker pull nousresearch/hermes-agent:latest"},
		{"clone NousResearch/hermes-agent into ~/.hermes/hermes-agent", "clone NousResearch/hermes-agent into ~/.icebath/icebath-agent"},
		{"exec /opt/hermes/docker/main-wrapper.sh hermes gateway", "exec /opt/hermes/docker/main-wrapper.sh icebath gateway"},
		{"model: NousResearch/Nous-Hermes-llama-1b-v1", "model: NousResearch/Nous-Hermes-llama-1b-v1"},
		{"# 5. GODMODE FAST — Hermes 4", "# 5. GODMODE FAST — Hermes 4"},
		{"ULTRAPLINIAN or use Hermes/Grok models", "ULTRAPLINIAN or use Hermes/Grok models"},
		{"- **Zero-refusal fast** (Hermes) — uncensored model", "- **Zero-refusal fast** (Hermes) — uncensored model"},
		{"---\nname: x\nauthor: Teknium (teknium1), Hermes Agent\nrequires: hermes\n", "---\nname: x\nauthor: Teknium (teknium1), Hermes Agent\nrequires: icebath\n"},
		{"# MIT License. Part of the Hermes docx skill.\nHERMES_X=1", "# MIT License. Part of the Hermes docx skill.\nICEBATH_X=1"},
	}
	for _, c := range cases {
		if got := renameText(c.in); got != c.want {
			t.Errorf("renameText(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestRenameArchiveFilesRenamesPathsAndKeepsBinaryAndModes(t *testing.T) {
	binary := []byte{0x89, 'P', 'N', 'G', 0, 'h', 'e', 'r', 'm', 'e', 's'}
	files := []tarzst.File{
		tarzst.NewFile("productivity/google-workspace/scripts/_hermes_home.py", []byte("HERMES_HOME\n"), 0o755),
		tarzst.NewFile("creative/x/logo.png", binary, 0o644),
	}
	out, err := renameArchiveFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Path != "productivity/google-workspace/scripts/_icebath_home.py" || string(out[0].Data) != "ICEBATH_HOME\n" || out[0].Mode != 0o755 {
		t.Fatalf("script renamed to %s %q %04o", out[0].Path, out[0].Data, out[0].Mode)
	}
	if out[1].Path != "creative/x/logo.png" || string(out[1].Data) != string(binary) || out[1].Sum != files[1].Sum {
		t.Fatalf("binary changed %s %q", out[1].Path, out[1].Data)
	}
	if _, err := renameArchiveFiles([]tarzst.File{tarzst.NewFile("a/hermes.md", nil, 0o644), tarzst.NewFile("a/icebath.md", nil, 0o644)}); err == nil ||
		!strings.Contains(err.Error(), "both rename to a/icebath.md") {
		t.Fatalf("collision not refused: %v", err)
	}
}

func TestPackOfUnpackedArchiveIsByteIdentical(t *testing.T) {
	files := []tarzst.File{tarzst.NewFile("b/run.sh", []byte("#!/bin/sh\n"), 0o755), tarzst.NewFile("a/SKILL.md", []byte("x\n"), 0o644)}
	dir := t.TempDir()
	archive := filepath.Join(dir, "in.tar.zst")
	if err := writeArchive(archive, "", files); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(dir, "tree")
	if err := unpackVerb([]string{"-archive", archive, "-dir", tree}); err != nil {
		t.Fatal(err)
	}
	repacked := filepath.Join(dir, "out.tar.zst")
	manifest := filepath.Join(dir, "manifest.txt")
	if err := packVerb([]string{"-dir", tree, "-out", repacked, "-manifest", manifest}); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(archive)
	b, _ := os.ReadFile(repacked)
	if string(a) != string(b) {
		t.Fatal("repacked archive differs from the archive it was unpacked from")
	}
	text, _ := os.ReadFile(manifest)
	if string(text) != tarzst.Manifest(files) {
		t.Fatalf("manifest %q", text)
	}
}
