package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Writes the catalog document of a plugin catalog directory, every entry file and the removed list, stamped now
func catalogVerb(args []string) error {
	fs := flag.NewFlagSet("catalog", flag.ExitOnError)
	dir := fs.String("dir", "", "plugin catalog directory of yaml entries and removed.yaml")
	out := fs.String("out", "", "catalog document to write")
	fs.Parse(args)
	if *dir == "" || *out == "" {
		return fmt.Errorf("catalog needs -dir and -out")
	}
	files, err := filepath.Glob(filepath.Join(*dir, "*.yaml"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	entries := []any{}
	removed := []any{}
	for _, path := range files {
		var doc map[string]any
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if filepath.Base(path) == "removed.yaml" {
			list, ok := doc["removed"].([]any)
			if !ok {
				return fmt.Errorf("%s: removed must be a list", path)
			}
			removed = list
			continue
		}
		if name, _ := doc["name"].(string); name+".yaml" != filepath.Base(path) {
			return fmt.Errorf("%s: name %q does not match the file name", path, doc["name"])
		}
		entries = append(entries, doc)
	}
	if len(entries) == 0 {
		return fmt.Errorf("%s holds no catalog entries", *dir)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	doc := map[string]any{"generated_at": time.Now().UTC().Format(time.RFC3339), "entries": entries, "removed": removed}
	if err := enc.Encode(doc); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(*out, buf.Bytes(), 0o644)
}
