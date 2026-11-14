package manage_test

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/backup"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
)

// Points ICEBATH_HOME at a source-home under the test dir and returns it
func transferHome(t *testing.T) string {
	t.Helper()
	tmp := manage.ManageEnv(t)
	src := filepath.Join(tmp, "source-home")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	switchHome(t, src)
	t.Cleanup(backup.SwapMemoryProviderExternalPaths(func(context.Context, string) []string { return nil }))
	t.Cleanup(backup.SwapGatewayService(func(context.Context) (bool, error) { return false, nil },
		func(context.Context, io.Writer, gateway.ServiceSetupOptions) (bool, error) { return false, nil }))
	return src
}

// Switches ICEBATH_HOME and drops the caches keyed by the previous home
func switchHome(t *testing.T, h string) {
	t.Helper()
	t.Setenv(home.EnvHome, h)
	profiles.ResetFileCache()
	profiles.ResetStandaloneMemo()
	home.ResetKeyCache()
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		manage.WriteFile(t, filepath.Join(root, filepath.FromSlash(rel)), content)
	}
}

// Top level components of every relative path, sorted
func topLevelParts(sets ...map[string]string) []string {
	seen := map[string]bool{}
	for _, s := range sets {
		for rel := range s {
			seen[strings.Split(rel, "/")[0]] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func writeDistManifest(t *testing.T, dir, name string, owned []string) {
	t.Helper()
	m := profiles.NewDistributionManifest(name)
	m.DistributionOwned = owned
	if _, err := profiles.WriteManifest(context.Background(), dir, m); err != nil {
		t.Fatal(err)
	}
}

func TestTransfersPreserveUserFilesWithoutPortingPMState(t *testing.T) {
	for _, route := range []string{"backup", "default", "named", "distribution", "explicit"} {
		t.Run(route, func(t *testing.T) {
			src := transferHome(t)
			tmp := filepath.Dir(src)
			manage.CaptureOut(t)
			ctx := context.Background()
			portable := map[string]string{
				"config.yaml":                    "model:\n  model: portable\n",
				"SOUL.md":                        "Be kind.\n",
				"plugins/example/facts.json":     "{\"owner\": \"plugin\"}\n",
				"skills/example/tools/helper.py": "print('portable tool')\n",
				"skills/example/cache/notes.txt": "user-authored data\n",
			}
			runtime := map[string]string{
				"installs/install-id/facts.json":                         "{\"environment\": \"C:/other-machine/generation\"}\n",
				"installs/install-id/environments/generation/pyvenv.cfg": "home = other-machine\n",
				"tools/uv/bin/uv":                 "machine executable\n",
				"cache/partials/transfer.partial": "unfinished transfer\n",
			}
			source := src
			if route == "named" {
				dir, err := profiles.GetProfileDir("porter")
				if err != nil {
					t.Fatal(err)
				}
				source = dir
			}
			writeFiles(t, source, portable)
			writeFiles(t, source, runtime)
			var archive string
			if route == "backup" {
				for _, set := range []map[string]string{portable, runtime} {
					for rel, text := range maps.Clone(set) {
						set["profiles/coder/"+rel] = text
					}
				}
				writeFiles(t, source, portable)
				writeFiles(t, source, runtime)
				archive = filepath.Join(tmp, "portable.zip")
				owedBackupStep(t, func() error {
					_, err := backup.RunBackup(ctx, home.Dir(ctx), map[string]any{"output": archive}, io.Discard)
					return err
				})
				names := zipNames(t, archive)
				for rel := range portable {
					if !names[rel] {
						t.Fatalf("%s missing from the archive", rel)
					}
				}
				for rel := range runtime {
					if names[rel] {
						t.Fatalf("%s shipped in the archive", rel)
					}
				}
			} else if route == "default" || route == "named" {
				name := map[bool]string{true: "porter", false: "default"}[route == "named"]
				out, err := manage.ExportProfile(name, filepath.Join(tmp, "portable.tar.gz"), nil)
				if err != nil {
					t.Fatal(err)
				}
				archive = out
				files, _ := manage.ArchiveMembers(t, archive)
				names := map[string]bool{}
				for n := range files {
					names[strings.TrimPrefix(n, name+"/")] = true
				}
				for rel := range portable {
					if !names[rel] {
						t.Fatalf("%s missing from the archive", rel)
					}
				}
				for rel := range runtime {
					if names[rel] {
						t.Fatalf("%s shipped in the archive", rel)
					}
				}
			} else {
				var owned []string
				if route == "explicit" {
					owned = topLevelParts(portable, runtime)
				}
				writeDistManifest(t, source, "portable", owned)
			}
			target := filepath.Join(tmp, "target-home")
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
			switchHome(t, target)
			var restored string
			if route == "backup" {
				owedBackupStep(t, func() error {
					_, err := backup.RunImport(ctx, home.Dir(ctx), map[string]any{"zipfile": archive, "force": true}, io.Discard)
					return err
				})
				restored = target
			} else if archive != "" {
				out, err := manage.ImportProfile(archive, "restored")
				if err != nil {
					t.Fatal(err)
				}
				restored = out
			} else {
				plan, err := manage.InstallDistribution(ctx, source, "restored", false, false)
				if err != nil {
					t.Fatal(err)
				}
				restored = plan.TargetDir
			}
			for rel, content := range portable {
				if got := manage.ReadText(t, filepath.Join(restored, filepath.FromSlash(rel))); got != content {
					t.Fatalf("%s %s = %q", route, rel, got)
				}
			}
			for rel, content := range runtime {
				if manage.Exists(filepath.Join(restored, filepath.FromSlash(rel))) {
					t.Fatalf("%s %s restored", route, rel)
				}
				if manage.ReadText(t, filepath.Join(source, filepath.FromSlash(rel))) != content {
					t.Fatalf("%s source %s changed", route, rel)
				}
			}
		})
	}
}

// Runs a backup route step whose hermes code is still owed, failing the subtest with the owed name
func owedBackupStep(t *testing.T, step func() error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%v", r)
		}
	}()
	if err := step(); err != nil {
		t.Fatal(err)
	}
}

// Writes a zip holding each member text under its name
func writeZip(t *testing.T, archive string, members map[string]string) {
	t.Helper()
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for _, name := range slices.Sorted(maps.Keys(members)) {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(fw, members[name]); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []interface{ Close() error }{w, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// Member names of a zip archive
func zipNames(t *testing.T, archive string) map[string]bool {
	t.Helper()
	r, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	names := map[string]bool{}
	for _, f := range r.File {
		names[f.Name] = true
	}
	return names
}

// Writes a tar.gz of dir with every member under arcname like tarfile.add
func tarDirectory(t *testing.T, dir, arcname, out string) {
	t.Helper()
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		hdr.Name = arcname
		if rel != "." {
			hdr.Name = arcname + "/" + filepath.ToSlash(rel)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.Type().IsRegular() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			_, err = tw.Write(data)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []interface{ Close() error }{tw, gz, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIncomingRuntimeStateNeverReplacesTargetRuntime(t *testing.T) {
	for _, route := range []string{"backup", "profile", "profile-denied", "distribution", "explicit"} {
		t.Run(route, func(t *testing.T) {
			src := transferHome(t)
			tmp := filepath.Dir(src)
			manage.CaptureOut(t)
			ctx := context.Background()
			incoming := map[string]string{
				"config.yaml":                    "model:\n  model: restored\n",
				"plugins/example/facts.json":     "{\"owner\": \"plugin\"}\n",
				"skills/example/tools/helper.py": "print('user tool')\n",
				"skills/example/cache/notes.txt": "keep user files\n",
			}
			machine := map[string]string{
				"installs/key/facts.json": "foreign generation\n",
				"tools/uv/bin/uv":         "foreign executable\n",
				"cache":                   "foreign cache file\n",
			}
			if route == "backup" {
				for _, set := range []map[string]string{incoming, machine} {
					for rel, text := range maps.Clone(set) {
						set["profiles/coder/"+rel] = text
					}
				}
			}
			staged := filepath.Join(tmp, "archive-source")
			writeFiles(t, staged, incoming)
			writeFiles(t, staged, machine)
			target, err := profiles.GetProfileDir("restored")
			if err != nil {
				t.Fatal(err)
			}
			if route == "backup" {
				target = src
			}
			profileRoute := strings.HasPrefix(route, "profile")
			if !profileRoute {
				for rel := range machine {
					manage.WriteFile(t, filepath.Join(target, filepath.FromSlash(rel)), "target-owned runtime\n")
				}
			}
			backupArchive := filepath.Join(tmp, "legacy.zip")
			if route == "backup" {
				members := map[string]string{}
				for _, set := range []map[string]string{incoming, machine} {
					for rel, text := range set {
						members[".icebath/"+rel] = text
					}
				}
				writeZip(t, backupArchive, members)
				owedBackupStep(t, func() error {
					_, err := backup.RunImport(ctx, home.Dir(ctx), map[string]any{"zipfile": backupArchive, "force": true}, io.Discard)
					return err
				})
			} else if profileRoute {
				archive := filepath.Join(tmp, "legacy.tar.gz")
				tarDirectory(t, staged, "legacy", archive)
				if route == "profile-denied" {
					var old func(string) error
					old = manage.SwapRmtreeStagedRuntime(func(p string) error {
						if filepath.Base(p) == "installs" {
							return fmt.Errorf("staged runtime is locked: %w", fs.ErrPermission)
						}
						return old(p)
					})
					_, err := manage.ImportProfile(archive, "restored")
					manage.SwapRmtreeStagedRuntime(old)
					if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "staged runtime is locked") {
						t.Fatalf("import: %v", err)
					}
					if manage.Exists(target) {
						t.Fatal("a refused import left the target behind")
					}
					return
				}
				if target, err = manage.ImportProfile(archive, "restored"); err != nil {
					t.Fatal(err)
				}
			} else {
				var owned []string
				if route == "explicit" {
					owned = topLevelParts(incoming, machine)
				}
				writeDistManifest(t, staged, "legacy", owned)
				plan, err := manage.InstallDistribution(ctx, staged, "restored", true, false)
				if err != nil {
					t.Fatal(err)
				}
				target = plan.TargetDir
			}
			for rel, content := range incoming {
				if got := manage.ReadText(t, filepath.Join(target, filepath.FromSlash(rel))); got != content {
					t.Fatalf("%s %s = %q", route, rel, got)
				}
			}
			for rel, content := range machine {
				p := filepath.Join(target, filepath.FromSlash(rel))
				if route == "profile" {
					if manage.Exists(p) {
						t.Fatalf("%s imported", rel)
					}
				} else if manage.ReadText(t, p) != "target-owned runtime\n" {
					t.Fatalf("%s %s replaced", route, rel)
				}
				if manage.ReadText(t, filepath.Join(staged, filepath.FromSlash(rel))) != content {
					t.Fatalf("staged %s changed", rel)
				}
			}
			if route == "backup" {
				writeZip(t, backupArchive, map[string]string{".icebath/config.yaml": incoming["config.yaml"],
					".icebath/skills/../installs/key/facts.json": "foreign generation\n"})
				owedBackupStep(t, func() error {
					_, err := backup.RunImport(ctx, home.Dir(ctx), map[string]any{"zipfile": backupArchive, "force": true}, io.Discard)
					return err
				})
				if manage.ReadText(t, filepath.Join(target, "installs", "key", "facts.json")) != "target-owned runtime\n" {
					t.Fatal("an in-home traversal bypassed the runtime root classification")
				}
			}
			if route == "explicit" {
				var owned []string
				for rel := range incoming {
					owned = append(owned, strings.ReplaceAll(rel, "/", "\\"))
				}
				for rel := range machine {
					owned = append(owned, strings.ReplaceAll(rel, "/", "\\"))
				}
				writeDistManifest(t, staged, "legacy", owned)
				if _, err := manage.InstallDistribution(ctx, staged, "restored", true, false); err != nil {
					t.Fatal(err)
				}
				for rel := range machine {
					if manage.ReadText(t, filepath.Join(target, filepath.FromSlash(rel))) != "target-owned runtime\n" {
						t.Fatalf("explicit backslash owned %s replaced", rel)
					}
				}
			}
		})
	}
}
