package plugins

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

	"github.com/nickheyer/icebath/pkg/pm"
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
	return homeCtx(h), h
}

// Plugin dir with a manifest and, unless plain, a pyproject declaring a dependency
func historicPlugin(t *testing.T, h, name string, dependencies bool) string {
	t.Helper()
	path := filepath.Join(h, "plugins", name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(path, "plugin.yaml"), "name: "+name+"\n")
	if dependencies {
		writeFile(t, filepath.Join(path, "pyproject.toml"), "[project]\ndependencies = [\"requests>=2\"]\n")
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

// Port of test_candidate_member_dirs_preserves_proposed_home_order_and_extras
func TestCandidateMemberDirsPreservesProposedHomeOrderAndExtras(t *testing.T) {
	for _, active := range []string{"", "default", "profile"} {
		t.Run("active="+active, func(t *testing.T) {
			ctx, h := historicHome(t)
			profile := filepath.Join(h, "profiles", "coder")
			for _, dir := range []string{h, profile} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(dir, "config.yaml"), "plugins:\n  enabled: [old]\n")
				historicPlugin(t, dir, "old", true)
				historicPlugin(t, dir, "new", true)
				historicPlugin(t, dir, "blocked", true)
				historicPlugin(t, dir, "plain", false)
			}
			extra := historicPlugin(t, h, "extra", true)
			selected := h
			if active == "profile" {
				selected = profile
			}
			expected := []string{filepath.Join(h, "plugins", "old"), filepath.Join(profile, "plugins", "old")}
			activeDir := ""
			if active != "" {
				i := 0
				if active == "profile" {
					i = 1
				}
				expected[i] = filepath.Join(selected, "plugins", "new")
				activeDir = filepath.Join(selected, "plugins")
			}
			before := treeBytes(t, h)
			got, err := CandidateMemberDirs(ctx, []string{"new", "blocked", "plain", "new"}, []string{"blocked"}, activeDir,
				[]string{expected[0], extra, extra, filepath.Join(h, "plugins", "plain"), filepath.Join(h, "missing")})
			if err != nil {
				t.Fatal(err)
			}
			if want := append(expected, extra); !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
			if !reflect.DeepEqual(treeBytes(t, h), before) {
				t.Fatal("candidate discovery wrote into the home")
			}
		})
	}
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
	writeFile(t, filepath.Join(target, "version"), "new code")
	backup := historicPlugin(t, h, ".previous-historic", true)
	writeFile(t, filepath.Join(backup, "version"), "old code")
	metadata := filepath.Join(filepath.Dir(target), ".install-metadata.json")
	writeFile(t, metadata, "{\"published\": \"new\"}\n")
	journal := filepath.Join(root, "historic-publication.json")
	writeFile(t, journal, "already decoded by the old caller")
	canonical := filepath.Join(pm.InstallStateDir(ctx, project), "publication.json")
	if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, canonical, "unrelated publication; do not read or remove")
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
				facts := pm.EnvironmentRuntimeFactsPath(ctx, project)
				if err := os.MkdirAll(filepath.Dir(facts), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, facts, "new facts")
			}
			if err := RecoverPluginPublication(ctx, project, row, journal); err != nil {
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
	if err := RecoverPluginPublication(ctx, project, row, journal); err != nil {
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
				writeFile(t, row["metadata"].(string), "independent user edit")
			} else {
				outside := filepath.Join(root, "outside", "plugins")
				if err := os.MkdirAll(outside, 0o755); err != nil {
					t.Fatal(err)
				}
				row[invalid] = filepath.Join(outside, filepath.Base(row[invalid].(string)))
			}
			before := treeBytes(t, root)
			err := RecoverPluginPublication(ctx, project, row, journal)
			if err == nil || !(strings.Contains(err.Error(), "paths escape") || strings.Contains(err.Error(), "metadata changed")) {
				t.Fatalf("error %v", err)
			}
			if !reflect.DeepEqual(treeBytes(t, root), before) || !histExists(journal) {
				t.Fatal("refused recovery wrote or removed the journal")
			}
		})
	}
}
