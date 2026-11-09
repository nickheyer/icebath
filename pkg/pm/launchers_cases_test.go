package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
)

// Shell stand in for the install binary that prints its arguments as a JSON list
const argvJSONBinary = `#!/bin/sh
out=""
for a in "$@"; do
  esc=$(printf %s "$a" | sed 's/\\/\\\\/g; s/"/\\"/g')
  out="$out\"$esc\","
done
printf '[%s]\n' "${out%,}"
`

// Runs a published command and returns the arguments the fake binary received
func runWrapper(t *testing.T, path string, args ...string) []string {
	t.Helper()
	out, err := exec.Command(path, args...).Output()
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var argv []string
	if err := json.Unmarshal(out, &argv); err != nil {
		t.Fatalf("%s printed %q: %v", path, out, err)
	}
	return argv
}

func mustWrite(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// test_post_update_expose_cli.py TestExposeCli test_unstamped_tree_with_sibling_bin_is_not_a_bundle
func TestExposeCliUnstampedTreeWithSiblingBinIsNotABundle(t *testing.T) {
	userHome, _ := lnFakeInstall(t)
	parent := filepath.Join(userHome, "icebath-home")
	mustWrite(t, filepath.Join(parent, "bin", "icebath"), "#!/bin/sh\n# installer launcher\n", 0o755)
	checkout := filepath.Join(parent, "icebath-agent")
	if err := os.MkdirAll(checkout, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvInstallRoot, checkout)
	if got := lnExpose(t, true); !reflect.DeepEqual(got, ExposeResult{OK: true, Skipped: "no-install-binary"}) {
		t.Fatalf("result %+v", got)
	}
	if IsBundledPayload(checkout) {
		t.Fatal("a sibling bin made the checkout a bundle")
	}
}

// test_post_update_expose_cli.py TestExposeCli test_replaces_a_dangling_symlink_from_old_installs
func TestExposeCliReplacesADanglingSymlinkFromOldInstalls(t *testing.T) {
	userHome, root := lnFakeInstall(t)
	wrapperDir := filepath.Join(userHome, ".local", "bin")
	console := filepath.Join(root, "venv", "bin", "icebath")
	mustWrite(t, console, "# real console script\n", 0o755)
	if err := os.MkdirAll(wrapperDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(console, filepath.Join(wrapperDir, "icebath")); err != nil {
		t.Fatal(err)
	}
	if got := lnExpose(t, true); !slices.Contains(got.Written, "icebath") {
		t.Fatalf("result %+v", got)
	}
	if mustRead(t, console) != "# real console script\n" || isSymlink(filepath.Join(wrapperDir, "icebath")) {
		t.Fatal("the old symlink was followed instead of replaced")
	}
}

// Sealed bundle payload bin holding the three fake shims, with the user home isolated
func sealedPayload(t *testing.T) (string, string) {
	t.Helper()
	userHome, _ := lnFakeInstall(t)
	payloadBin := filepath.Join(userHome, "Icebath.app", "Contents", "Resources", "agent-payload", "bin")
	for _, name := range []string{"icebath", "icebath-agent", "icebath-acp"} {
		mustWrite(t, filepath.Join(payloadBin, name), "fake mach-o shim\n", 0o755)
	}
	return userHome, payloadBin
}

func sortedWritten(result ExposeResult) []string {
	written := append([]string{}, result.Written...)
	slices.Sort(written)
	return written
}

func readLink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

// test_post_update_expose_cli.py TestSymlinkSealedLaunchers test_links_all_three_fresh
func TestSymlinkSealedLaunchersLinksAllThreeFresh(t *testing.T) {
	userHome, payloadBin := sealedPayload(t)
	result := symlinkSealedLaunchers(payloadBin)
	if !result.OK || !reflect.DeepEqual(sortedWritten(result), []string{"icebath", "icebath-acp", "icebath-agent"}) {
		t.Fatalf("result %+v", result)
	}
	for _, name := range []string{"icebath", "icebath-agent", "icebath-acp"} {
		if link := filepath.Join(userHome, ".local", "bin", name); readLink(t, link) != filepath.Join(payloadBin, name) {
			t.Fatalf("%s links %s", name, readLink(t, link))
		}
	}
}

// test_post_update_expose_cli.py TestSymlinkSealedLaunchers test_second_run_is_a_no_op
func TestSymlinkSealedLaunchersSecondRunIsANoOp(t *testing.T) {
	_, payloadBin := sealedPayload(t)
	symlinkSealedLaunchers(payloadBin)
	if result := symlinkSealedLaunchers(payloadBin); len(result.Written) != 0 {
		t.Fatalf("second run wrote %v", result.Written)
	}
}

// test_post_update_expose_cli.py TestSymlinkSealedLaunchers test_retargets_own_link_after_app_moved
func TestSymlinkSealedLaunchersRetargetsOwnLinkAfterAppMoved(t *testing.T) {
	userHome, payloadBin := sealedPayload(t)
	linkDir := filepath.Join(userHome, ".local", "bin")
	if err := os.MkdirAll(linkDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(payloadBin), "bin-old", "icebath"), filepath.Join(linkDir, "icebath")); err != nil {
		t.Fatal(err)
	}
	result := symlinkSealedLaunchers(payloadBin)
	if !slices.Contains(result.Written, "icebath") || readLink(t, filepath.Join(linkDir, "icebath")) != filepath.Join(payloadBin, "icebath") {
		t.Fatalf("result %+v link %s", result, readLink(t, filepath.Join(linkDir, "icebath")))
	}
}

// test_post_update_expose_cli.py TestSymlinkSealedLaunchers test_never_touches_a_live_foreign_entry
func TestSymlinkSealedLaunchersNeverTouchesALiveForeignEntry(t *testing.T) {
	userHome, payloadBin := sealedPayload(t)
	linkDir := filepath.Join(userHome, ".local", "bin")
	mustWrite(t, filepath.Join(linkDir, "icebath"), "#!/bin/sh\n# pipx launcher\n", 0o755)
	other := filepath.Join(userHome, "other-tool")
	mustWrite(t, other, "other\n", 0o755)
	if err := os.Symlink(other, filepath.Join(linkDir, "icebath-agent")); err != nil {
		t.Fatal(err)
	}
	result := symlinkSealedLaunchers(payloadBin)
	if mustRead(t, filepath.Join(linkDir, "icebath")) != "#!/bin/sh\n# pipx launcher\n" || readLink(t, filepath.Join(linkDir, "icebath-agent")) != other {
		t.Fatal("a live foreign entry was touched")
	}
	if !reflect.DeepEqual(sortedWritten(result), []string{"icebath-acp"}) {
		t.Fatalf("written %v", result.Written)
	}
}

// test_post_update_expose_cli.py test_environment_block_reader_counts_utf16_units
func TestEnvironmentBlockReaderCountsUtf16Units(t *testing.T) {
	block := utf16.Encode([]rune("A=1\x00MOOD=\U0001F600\x00ICEBATH_HOME=C:\\h\x00=C:=C:\\\x00\x00"))
	want := map[string]string{"A": "1", "MOOD": "\U0001F600", "ICEBATH_HOME": "C:\\h"}
	if got := readEnvironmentBlock(block); !reflect.DeepEqual(got, want) {
		t.Fatalf("environ %v", got)
	}
}

// Publishes the install's own commands into the root, returning each shared wrapper's bytes by name
func publishOwn(t *testing.T, root string) map[string]string {
	t.Helper()
	written, err := EnsureInstallLaunchers(context.Background(), root, root)
	if err != nil || len(written) != len(windowsBinLaunchers) {
		t.Fatalf("written %v err %v", written, err)
	}
	wrappers := map[string]string{}
	for _, path := range written {
		if filepath.Base(path) != installBinaryName() {
			wrappers[filepath.Base(path)] = mustRead(t, path)
		}
	}
	return wrappers
}

// test_hermes_home_shared_launchers.py test_missing_dead_and_same_store_launchers_still_publish
func TestMissingDeadAndSameStoreLaunchersStillPublish(t *testing.T) {
	userHome, root := lnFakeInstall(t)
	binary := filepath.Join(root, "icebath")
	before := publishOwn(t, root)
	if len(before) == 0 {
		t.Fatal("no shared command published")
	}
	mustWrite(t, binary, "\x7fELF repinned\n", 0o755)
	after := publishOwn(t, root)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("a same install repin rewrote the shared commands %v", after)
	}
	for name := range after {
		if launcherBinary(filepath.Join(root, name)) != binary {
			t.Fatalf("%s runs %s", name, launcherBinary(filepath.Join(root, name)))
		}
	}
	gone := filepath.Join(userHome, "gone", "icebath")
	if writeShell(filepath.Join(root, "icebath-acp"), []string{gone, "acp"}) == "" {
		t.Fatal("dead wrapper not written")
	}
	t.Setenv(home.EnvHome, filepath.Join(userHome, "temp-home"))
	healed := publishOwn(t, root)
	for name := range healed {
		if launcherBinary(filepath.Join(root, name)) != binary {
			t.Fatalf("%s still runs %s after its binary died", name, launcherBinary(filepath.Join(root, name)))
		}
	}
}

// test_source_launcher_publication.py test_posix_materializer_publishes_only_executable_shell_launchers
func TestPosixMaterializerPublishesOnlyExecutableShellLaunchers(t *testing.T) {
	for _, corruption := range []string{"bom", "crlf"} {
		t.Run(corruption, func(t *testing.T) {
			userHome, root := lnFakeInstall(t)
			ctx := context.Background()
			out := filepath.Join(userHome, "bin")
			if err := os.MkdirAll(out, 0o777); err != nil {
				t.Fatal(err)
			}
			written, err := EnsureInstallLaunchers(ctx, root, out)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, path := range written {
				if !executable(path) || filepath.Dir(path) != out {
					t.Fatalf("published %s", path)
				}
				names = append(names, filepath.Base(path))
			}
			slices.Sort(names)
			entries, _ := os.ReadDir(out)
			if !reflect.DeepEqual(names, []string{"icebath", "icebath-acp"}) || len(entries) != len(written) {
				t.Fatalf("names %v entries %d", names, len(entries))
			}
			local, _ := os.ReadDir(root)
			var localNames []string
			for _, e := range local {
				localNames = append(localNames, e.Name())
			}
			slices.Sort(localNames)
			if !reflect.DeepEqual(localNames, []string{"icebath", "icebath-acp"}) {
				t.Fatalf("own commands %v", localNames)
			}
			launcher := filepath.Join(root, "icebath-acp")
			expected := mustRead(t, launcher)
			corrupted := "\xef\xbb\xbf" + expected
			if corruption == "crlf" {
				corrupted = strings.ReplaceAll(expected, "\n", "\r\n")
			}
			mustWrite(t, launcher, corrupted, 0o755)
			if written, err := EnsureInstallLaunchers(ctx, root, out); err != nil || len(written) == 0 {
				t.Fatalf("republish %v %v", written, err)
			}
			if mustRead(t, launcher) != expected {
				t.Fatalf("corrupted launcher kept %q", mustRead(t, launcher))
			}
			var stdout, stderr bytes.Buffer
			if code := printRuntimeCommand(root, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("print-runtime-command exited %d %s", code, stderr.String())
			}
			var command []string
			if err := json.Unmarshal(stdout.Bytes(), &command); err != nil || len(command) == 0 {
				t.Fatalf("command %q %v", stdout.String(), err)
			}
			if a, err := os.Stat(command[0]); err != nil {
				t.Fatal(err)
			} else if b, _ := os.Stat(filepath.Join(root, "icebath")); !os.SameFile(a, b) {
				t.Fatalf("command runs %s", command[0])
			}
			before := lnMtimes(t, launcher)
			if written, err := EnsureInstallLaunchers(ctx, root, out); err != nil || len(written) == 0 {
				t.Fatalf("third publish %v %v", written, err)
			}
			if !reflect.DeepEqual(lnMtimes(t, launcher), before) {
				t.Fatal("an unchanged launcher was rewritten")
			}
		})
	}
}

// test_source_launcher_publication.py test_materializer_cli_refuses_missing_store_without_publishing
func TestMaterializerCliRefusesMissingStoreWithoutPublishing(t *testing.T) {
	userHome, _ := lnFakeInstall(t)
	root := filepath.Join(userHome, "bare")
	if err := os.MkdirAll(root, 0o777); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(userHome, "bin")
	var stdout, stderr bytes.Buffer
	if code := LaunchersMain(context.Background(), root, []string{out}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "store interpreter") {
		t.Fatalf("exited %d %q", code, stderr.String())
	}
	if entries, err := os.ReadDir(out); err == nil && len(entries) != 0 {
		t.Fatalf("published %d entries without a store", len(entries))
	}
}

// test_source_launcher_publication.py test_boot_migrates_legacy_conveniences_to_selected_runtime
func TestBootMigratesLegacyConveniencesToSelectedRuntime(t *testing.T) {
	for _, legacy := range []string{"venv-python", "venv-console-script"} {
		t.Run(legacy, func(t *testing.T) {
			userHome, root := lnFakeInstall(t)
			binary := filepath.Join(root, "icebath")
			mustWrite(t, binary, argvJSONBinary, 0o755)
			out := filepath.Join(userHome, ".local", "bin")
			oldWrapper := "#!/bin/sh\nexec \"" + binary + "\" \"$@\"\n"
			if legacy == "venv-console-script" {
				oldWrapper = "#!/usr/bin/env bash\nunset PYTHONPATH\nunset PYTHONHOME\nexec \"" + binary + "\" \"$@\"\n"
			}
			mustWrite(t, filepath.Join(out, "icebath"), oldWrapper, 0o755)
			mustWrite(t, filepath.Join(out, "icebath-acp"), "#!/usr/bin/env bash\n# Icebath Agent ACP launcher (written by `icebath update`).\nexec \""+filepath.Join(out, "icebath")+"\" acp \"$@\"\n", 0o755)
			foreign := "#!/bin/sh\n# user note about " + binary + "\nexec \"" + filepath.Join(userHome, "other", "icebath") + "\" \"$@\"\n"
			mustWrite(t, filepath.Join(out, "icebath-agent"), foreign, 0o755)
			result := lnExpose(t, true)
			if !result.OK || !reflect.DeepEqual(sortedWritten(result), []string{"icebath", "icebath-acp"}) {
				t.Fatalf("result %+v", result)
			}
			if argv := runWrapper(t, filepath.Join(out, "icebath"), "quoted argument"); !reflect.DeepEqual(argv, []string{"quoted argument"}) {
				t.Fatalf("icebath argv %v", argv)
			}
			if argv := runWrapper(t, filepath.Join(out, "icebath-acp"), "quoted argument"); !reflect.DeepEqual(argv, []string{"acp", "quoted argument"}) {
				t.Fatalf("icebath-acp argv %v", argv)
			}
			if mustRead(t, filepath.Join(out, "icebath-agent")) != foreign {
				t.Fatal("the foreign command was rewritten")
			}
			paths := []string{filepath.Join(out, "icebath"), filepath.Join(out, "icebath-acp"), filepath.Join(out, "icebath-agent")}
			before := lnMtimes(t, paths...)
			if again := lnExpose(t, true); len(again.Written) != 0 || !reflect.DeepEqual(lnMtimes(t, paths...), before) {
				t.Fatalf("second exposure wrote %v", again.Written)
			}
		})
	}
}

// Store under a foreign runtime directory, named through ICEBATH_RUNTIME_DIR for the process
func foreignRuntimeStore(t *testing.T, dir string) string {
	t.Helper()
	mustWrite(t, filepath.Join(dir, "facts.json"), `{"packages": {"python": {"entry": "python-foreign"}}}`, 0o644)
	t.Setenv(EnvRuntimeDir, dir)
	return dir
}

// test_source_launcher_publication.py test_stage_launcher_ignores_an_inherited_runtime_directory
func TestStageLauncherIgnoresAnInheritedRuntimeDirectory(t *testing.T) {
	userHome, root := lnFakeInstall(t)
	foreign := foreignRuntimeStore(t, filepath.Join(userHome, "foreign", "tools"))
	published := stageLauncher("icebath-acp", root, root)
	if published == "" {
		t.Fatal("nothing published")
	}
	wrapper := mustRead(t, published)
	if !strings.Contains(wrapper, filepath.Join(root, "icebath")) || strings.Contains(wrapper, foreign) {
		t.Fatalf("wrapper %q", wrapper)
	}
	if stageLauncher("icebath", root, root) != filepath.Join(root, "icebath") {
		t.Fatal("the bare command must be the install binary itself")
	}
}

// test_source_launcher_publication.py test_install_launchers_never_bind_a_fixture_store_under_scratch
func TestInstallLaunchersNeverBindAFixtureStoreUnderScratch(t *testing.T) {
	_, root := lnFakeInstall(t)
	fixture := foreignRuntimeStore(t, filepath.Join(home.Root(), "cache", "scratch", "icebath-e2e-media-overlap-run", "icebath-home", "tools"))
	written, err := EnsureInstallLaunchers(context.Background(), root, root)
	if err != nil || len(written) != len(windowsBinLaunchers) {
		t.Fatalf("written %v err %v", written, err)
	}
	for _, path := range written {
		if filepath.Base(path) == installBinaryName() {
			continue
		}
		if wrapper := mustRead(t, path); strings.Contains(wrapper, "icebath-e2e-media-overlap-run") || strings.Contains(wrapper, fixture) || !strings.Contains(wrapper, pyshlex.Quote(filepath.Join(root, "icebath"))) {
			t.Fatalf("wrapper %q", wrapper)
		}
	}
}

// test_source_launcher_publication.py test_publication_uses_the_runtime_store_when_the_tree_records_none
func TestPublicationUsesTheRuntimeStoreWhenTheTreeRecordsNone(t *testing.T) {
	userHome, root := lnFakeInstall(t)
	ctx := context.Background()
	runtime := foreignRuntimeStore(t, filepath.Join(userHome, "runtime-tools"))
	if store, err := EnvironmentStoreRoot(ctx, root, true); err != nil || store != resolvePath(runtime) {
		t.Fatalf("store %q err %v", store, err)
	}
	written, err := EnsureInstallLaunchers(ctx, root, root)
	if err != nil || len(written) != len(windowsBinLaunchers) {
		t.Fatalf("written %v err %v", written, err)
	}
	for _, path := range written {
		if filepath.Base(path) != installBinaryName() && !strings.Contains(mustRead(t, path), pyshlex.Quote(filepath.Join(root, "icebath"))) {
			t.Fatalf("wrapper %q", mustRead(t, path))
		}
	}
}

// test_source_launcher_publication.py test_runtime_override_still_selects_the_runtime_python
func TestRuntimeOverrideStillSelectsTheRuntimePython(t *testing.T) {
	userHome, root := lnFakeInstall(t)
	foreign := foreignRuntimeStore(t, filepath.Join(userHome, "foreign", "tools"))
	selected, err := EnvironmentStoreRoot(context.Background(), root, true)
	if err != nil || selected != resolvePath(foreign) {
		t.Fatalf("selected %q err %v", selected, err)
	}
	if own, err := EnvironmentStoreRoot(context.Background(), root, false); err != nil || own == resolvePath(foreign) {
		t.Fatalf("publication store %q err %v", own, err)
	}
}

// test_source_launcher_publication.py test_pre_pm_base_dependencies_activate_only_at_boot
func TestPrePmBaseDependenciesActivateOnlyAtBoot(t *testing.T) {
	userHome, _ := lnFakeInstall(t)
	payload := filepath.Join(userHome, "payload")
	repo := filepath.Join(payload, "repo")
	mustWrite(t, filepath.Join(repo, "icebath"), "\x7fELF payload\n", 0o755)
	mustWrite(t, filepath.Join(payload, "manifest.json"), `{"repo": "repo", "venv": "payload-deps", "store": "tools"}`, 0o644)
	mustWrite(t, filepath.Join(payload, "payload-deps", "lib", "python3", "site-packages", "member.pth"), filepath.Join(userHome, "editable")+"\n", 0o644)
	t.Setenv("PYTHONPATH", filepath.Join(userHome, "ambient"))
	command := InstallRuntimeCommand(repo, nil, LaunchCommandOptions{})
	if !reflect.DeepEqual(command, []string{filepath.Join(repo, "icebath"), PinDefaultHomeFlag}) {
		t.Fatalf("command %v", command)
	}
	if strings.Contains(strings.Join(command, " "), "site-packages") || strings.Contains(strings.Join(command, " "), "ambient") {
		t.Fatal("the persisted command captured dependencies")
	}
}

// test_source_launcher_publication.py test_external_interpreter_keeps_its_owned_dependencies
func TestExternalInterpreterKeepsItsOwnedDependencies(t *testing.T) {
	userHome, _ := lnFakeInstall(t)
	root := filepath.Join(userHome, "external")
	if err := os.MkdirAll(root, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvRuntimeDir, filepath.Join(userHome, "empty-store"))
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := InstallRuntimeCommand(root, []string{"--version"}, LaunchCommandOptions{})
	if !reflect.DeepEqual(command, []string{self, PinDefaultHomeFlag, "--version"}) {
		t.Fatalf("command %v", command)
	}
	if !reflect.DeepEqual(InstallationCommand(root, []string{"--version"}, LaunchCommandOptions{}), command) {
		t.Fatal("a tree without its own binary must persist the external runtime command")
	}
}
