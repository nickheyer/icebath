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

// Writes the sorted digest and mode manifest of every regular file under payload, the tree the package embeds
func main() {
	files, err := collect("payload")
	if err != nil {
		fail(err)
	}
	var manifest bytes.Buffer
	for _, rel := range files {
		p := filepath.Join("payload", filepath.FromSlash(rel))
		data, err := os.ReadFile(p)
		if err != nil {
			fail(err)
		}
		info, err := os.Stat(p)
		if err != nil {
			fail(err)
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&manifest, "%s %s %s\n", rel, hex.EncodeToString(sum[:]), modeOf(info.Mode(), data))
	}
	if err := os.WriteFile("payload_manifest.txt", manifest.Bytes(), 0o644); err != nil {
		fail(err)
	}
}

// Sorted slash paths of every regular file under root
func collect(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
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
