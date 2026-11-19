package pm

import (
	"bytes"
	"context"
	"encoding/base64"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Resolved temp home set as ICEBATH_HOME with HOME at its parent, the isolated_home fixture
func historicHome(t *testing.T) (context.Context, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := filepath.Join(root, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("ICEBATH_HOME", h)
	return home.WithOverride(context.Background(), h), h
}

// Plugin dir with a manifest and, unless plain, a pyproject declaring a dependency
func historicPlugin(t *testing.T, h, name string, dependencies bool) string {
	t.Helper()
	path := filepath.Join(h, "plugins", name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(path, "plugin.yaml"), []byte("name: "+name+"\n"), 0o644)
	if dependencies {
		writeFile(t, filepath.Join(path, "pyproject.toml"), []byte("[project]\ndependencies = [\"requests>=2\"]\n"), 0o644)
	}
	return path
}

// Every regular file under a root mapped to its bytes
func treeBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		out[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The publication fixture of a shipped caller, project, row, journal and canonical journal paths
func historicPublication(t *testing.T) (context.Context, string, map[string]any, string, string, string) {
	t.Helper()
	ctx, h := historicHome(t)
	root := filepath.Dir(h)
	project := filepath.Join(root, "checkout")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	target := historicPlugin(t, h, "published", true)
	writeFile(t, filepath.Join(target, "version"), []byte("new code"), 0o644)
	backup := historicPlugin(t, h, ".previous-historic", true)
	writeFile(t, filepath.Join(backup, "version"), []byte("old code"), 0o644)
	metadata := filepath.Join(filepath.Dir(target), ".install-metadata.json")
	writeFile(t, metadata, []byte("{\"published\": \"new\"}\n"), 0o644)
	journal := filepath.Join(root, "historic-publication.json")
	writeFile(t, journal, []byte("already decoded by the old caller"), 0o644)
	canonical := filepath.Join(InstallStateDir(ctx, project), "publication.json")
	if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, canonical, []byte("unrelated publication; do not read or remove"), 0o644)
	row := map[string]any{"kind": "plugin", "target": target, "backup": backup, "metadata": metadata, "target_existed": true, "facts_before": nil,
		"metadata_before": base64.StdEncoding.EncodeToString([]byte("{\"published\": \"old\"}\n")),
		"metadata_after":  base64.StdEncoding.EncodeToString([]byte("{\"published\": \"new\"}\n"))}
	return ctx, project, row, journal, canonical, root
}

func histRead(path string) string { b, _ := os.ReadFile(path); return string(b) }

func histExists(path string) bool { _, err := os.Lstat(path); return err == nil }

// Port of test_old_publication_recovers_in_stdlib_using_supplied_row_and_journal
func TestOldPublicationRecoversUsingSuppliedRowAndJournal(t *testing.T) {
	for _, commit := range []string{"rollback", "explicit", "facts-changed"} {
		t.Run(commit, func(t *testing.T) {
			ctx, project, row, journal, canonical, _ := historicPublication(t)
			target, backup, metadata := row["target"].(string), row["backup"].(string), row["metadata"].(string)
			switch commit {
			case "explicit":
				row["committed"] = true
			case "facts-changed":
				facts := EnvironmentRuntimeFactsPath(ctx, project)
				if err := os.MkdirAll(filepath.Dir(facts), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, facts, []byte("new facts"), 0o644)
			}
			if err := recoverPluginPublication(ctx, project, row, journal); err != nil {
				t.Fatal(err)
			}
			wantCode, wantMeta := "new code", row["metadata_after"].(string)
			if commit == "rollback" {
				wantCode, wantMeta = "old code", row["metadata_before"].(string)
			}
			meta, _ := base64.StdEncoding.DecodeString(wantMeta)
			if got := histRead(filepath.Join(target, "version")); got != wantCode {
				t.Fatalf("version %q", got)
			}
			if got := histRead(metadata); !bytes.Equal([]byte(got), meta) {
				t.Fatalf("metadata %q", got)
			}
			if histExists(backup) || histExists(journal) || histRead(canonical) != "unrelated publication; do not read or remove" {
				t.Fatalf("backup %v journal %v canonical %q", histExists(backup), histExists(journal), histRead(canonical))
			}
		})
	}
}

// Port of test_old_publication_rolls_back_a_first_install
func TestOldPublicationRollsBackAFirstInstall(t *testing.T) {
	ctx, project, row, journal, _, _ := historicPublication(t)
	if err := os.RemoveAll(row["backup"].(string)); err != nil {
		t.Fatal(err)
	}
	row["target_existed"], row["metadata_before"] = false, nil
	if err := recoverPluginPublication(ctx, project, row, journal); err != nil {
		t.Fatal(err)
	}
	if histExists(row["target"].(string)) || histExists(row["metadata"].(string)) || histExists(journal) {
		t.Fatal("first install rollback left the target, metadata or journal")
	}
}

// Port of test_old_publication_refuses_unsafe_or_changed_state_without_writes
func TestOldPublicationRefusesUnsafeOrChangedStateWithoutWrites(t *testing.T) {
	for _, invalid := range []string{"target", "backup", "metadata", "edited-metadata"} {
		t.Run(invalid, func(t *testing.T) {
			ctx, project, row, journal, _, root := historicPublication(t)
			if invalid == "edited-metadata" {
				writeFile(t, row["metadata"].(string), []byte("independent user edit"), 0o644)
			} else {
				outside := filepath.Join(root, "outside", "plugins")
				if err := os.MkdirAll(outside, 0o755); err != nil {
					t.Fatal(err)
				}
				row[invalid] = filepath.Join(outside, filepath.Base(row[invalid].(string)))
			}
			before := treeBytes(t, root)
			err := recoverPluginPublication(ctx, project, row, journal)
			if err == nil || !(strings.Contains(err.Error(), "paths escape") || strings.Contains(err.Error(), "metadata changed")) {
				t.Fatalf("error %v", err)
			}
			if !reflect.DeepEqual(treeBytes(t, root), before) || !histExists(journal) {
				t.Fatal("refused recovery wrote or removed the journal")
			}
		})
	}
}
