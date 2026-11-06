package manage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Atomic rewrite of a third party JSON config, false when the filesystem refused it
func atomicWriteJSON(path string, data any) bool {
	return config.AtomicJSONWrite(path, data) == nil
}

// Renames the Honcho host blocks of a renamed profile in every Honcho config, keeping the AI peer identity
func migrateHonchoProfileHost(oldName, newName, newDir string) error {
	oldHost := "icebath_" + oldName
	legacyOldHost := "icebath." + oldName
	newHost := "icebath_" + newName
	candidates := []string{filepath.Join(newDir, "honcho.json"), filepath.Join(profiles.DefaultHome(), "honcho.json")}
	if accountHome, err := home.UserHome(); err == nil {
		candidates = append(candidates, filepath.Join(accountHome, ".honcho", "config.json"))
	}
	seen := map[string]bool{}
	for _, path := range candidates {
		resolved := resolvePath(path)
		if seen[resolved] {
			continue
		}
		if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
			continue
		}
		seen[resolved] = true
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if !utf8.Valid(raw) {
			return fmt.Errorf("'utf-8' codec can't decode the contents of %s", path)
		}
		doc, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\ufeff"))
		if err != nil {
			continue
		}
		root, ok := doc.(*pyjson.Object)
		if !ok {
			return fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(doc))
		}
		hostsV, _ := root.Get("hosts")
		hosts, ok := hostsV.(*pyjson.Object)
		if !ok {
			continue
		}
		source := oldHost
		if _, has := hosts.Get(oldHost); !has {
			source = legacyOldHost
		}
		block, has := hosts.Get(source)
		if !has {
			continue
		}
		if _, exists := hosts.Get(newHost); exists {
			fmt.Fprintf(stdout, "⚠ Honcho host block not migrated: %s already exists in %s\n", newHost, path)
			continue
		}
		if b, isObj := block.(*pyjson.Object); isObj {
			if _, hasPeer := b.Get("aiPeer"); !hasPeer {
				b.Set("aiPeer", oldName)
			}
		}
		hosts.Delete(source)
		hosts.Set(newHost, block)
		if atomicWriteJSON(path, root) {
			fmt.Fprintf(stdout, "✓ Honcho host updated: %s → %s\n", source, newHost)
		}
	}
	return nil
}
