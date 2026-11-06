package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Reference roots copied into the payload, each a project and its embedded entries
var sources = []struct {
	project string
	entries []string
}{
	{"elephant-goldfish-model", []string{"skills", "agents", "resources", "docs", "plugin.json", "README.md", "LICENSE", "TESTING.md", "AGENTS.md", "scripts/pack_plugin.py", "assets", ".codex-plugin/plugin.json"}},
	{"hallmark", []string{"skills", ".claude-plugin"}},
	{"impeccable", []string{"plugin", "skill"}},
}

// Copies the reference plugin files byte for byte into payload and writes the sorted digest manifest
func main() {
	refs := filepath.Join("..", "..", "references", "projects")
	if len(os.Args) > 1 {
		refs = os.Args[1]
	}
	files, err := collect(refs)
	if err != nil {
		fail(err)
	}
	if err := os.RemoveAll("payload"); err != nil {
		fail(err)
	}
	var manifest bytes.Buffer
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(refs, filepath.FromSlash(rel)))
		if err != nil {
			fail(err)
		}
		info, err := os.Stat(filepath.Join(refs, filepath.FromSlash(rel)))
		if err != nil {
			fail(err)
		}
		target := filepath.Join("payload", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			fail(err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			fail(err)
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&manifest, "%s %s %s\n", rel, hex.EncodeToString(sum[:]), modeOf(info.Mode(), data))
	}
	if err := os.WriteFile("payload_manifest.txt", manifest.Bytes(), 0o644); err != nil {
		fail(err)
	}
}

// Sorted slash paths of every regular file under the embedded reference entries
func collect(refs string) ([]string, error) {
	var out []string
	for _, src := range sources {
		for _, entry := range src.entries {
			root := filepath.Join(refs, src.project, entry)
			err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.Type().IsRegular() {
					return nil
				}
				rel, err := filepath.Rel(refs, p)
				if err != nil {
					return err
				}
				out = append(out, filepath.ToSlash(rel))
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// Materialized mode of a payload file, executable for launchers and shebang scripts
func modeOf(mode fs.FileMode, data []byte) string {
	if mode&0o100 != 0 || strings.HasPrefix(string(data), "#!") {
		return "0755"
	}
	return "0644"
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "genpayload:", err)
	os.Exit(1)
}
