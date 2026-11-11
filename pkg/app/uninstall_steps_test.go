package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
)

const uninstallZshrc = "export EDITOR=vim\n" +
	"alias ll='ls -la'\n" +
	"\n" +
	"# Icebath Agent\n" +
	"export PATH=\"$HOME/.local/bin:$PATH\"\n" +
	"\n" +
	"source ~/.work-profile\n"

// Points the user home and the icebath home at a throwaway dir
func uninstallFakeHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("ICEBATH_HOME", filepath.Join(h, ".icebath"))
	return h
}

func uninstallWrite(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func uninstallExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestHermesPathBlockIsRemoved(t *testing.T) {
	h := uninstallFakeHome(t)
	rc := filepath.Join(h, ".zshrc")
	uninstallWrite(t, rc, uninstallZshrc)
	removed := removePathFromShellConfigs(context.Background(), io.Discard)
	if !slices.Equal(removed, []string{rc}) {
		t.Fatalf("removed %v", removed)
	}
	text, _ := os.ReadFile(rc)
	if strings.Contains(string(text), "# Icebath Agent") || !strings.Contains(string(text), "export EDITOR=vim") ||
		!strings.Contains(string(text), "source ~/.work-profile") {
		t.Fatalf("rc %q", text)
	}
}

func TestUntouchedRcIsNotReported(t *testing.T) {
	h := uninstallFakeHome(t)
	rc := filepath.Join(h, ".zshrc")
	uninstallWrite(t, rc, "export EDITOR=vim\n")
	if removed := removePathFromShellConfigs(context.Background(), io.Discard); len(removed) != 0 {
		t.Fatalf("removed %v", removed)
	}
	if text, _ := os.ReadFile(rc); string(text) != "export EDITOR=vim\n" {
		t.Fatalf("rc %q", text)
	}
}

func TestAllShellRcCandidatesAreSwept(t *testing.T) {
	h := uninstallFakeHome(t)
	names := []string{".bashrc", ".bash_profile", ".profile", ".zshrc", ".zprofile"}
	for _, name := range names {
		uninstallWrite(t, filepath.Join(h, name), uninstallZshrc)
	}
	removed := removePathFromShellConfigs(context.Background(), io.Discard)
	var got []string
	for _, p := range removed {
		got = append(got, filepath.Base(p))
	}
	slices.Sort(got)
	want := slices.Clone(names)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("swept %v", got)
	}
}

func TestShellConfigSurvivesAnInterruptedRewrite(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("POSIX permission bits stop the rewrite here")
	}
	h := uninstallFakeHome(t)
	rc := filepath.Join(h, ".zshrc")
	uninstallWrite(t, rc, uninstallZshrc)
	original, _ := os.ReadFile(rc)
	if err := os.Chmod(h, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(h, 0o755) })
	var out bytes.Buffer
	if removed := removePathFromShellConfigs(context.Background(), &out); len(removed) != 0 {
		t.Fatalf("removed %v", removed)
	}
	if now, _ := os.ReadFile(rc); !bytes.Equal(now, original) {
		t.Fatalf("rc changed %q", now)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(h, "*.tmp")); len(leftovers) != 0 {
		t.Fatalf("temp files %v", leftovers)
	}
	if !strings.Contains(out.String(), "Could not update "+rc) {
		t.Fatalf("warning %q", out.String())
	}
}

func TestSymlinkedShellConfigStaysASymlink(t *testing.T) {
	h := uninstallFakeHome(t)
	real := filepath.Join(h, "dotfiles", "zshrc")
	uninstallWrite(t, real, uninstallZshrc)
	rc := filepath.Join(h, ".zshrc")
	if err := os.Symlink(real, rc); err != nil {
		t.Skipf("native symlink creation unavailable: %v", err)
	}
	removed := removePathFromShellConfigs(context.Background(), io.Discard)
	if !slices.Equal(removed, []string{rc}) {
		t.Fatalf("removed %v", removed)
	}
	if st, err := os.Lstat(rc); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a regular file")
	}
	text, _ := os.ReadFile(real)
	if strings.Contains(string(text), "# Icebath Agent") || !strings.Contains(string(text), "export EDITOR=vim") {
		t.Fatalf("real %q", text)
	}
}

func TestExistingFileModeIsPreserved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	h := uninstallFakeHome(t)
	rc := filepath.Join(h, ".zshrc")
	uninstallWrite(t, rc, uninstallZshrc)
	os.Chmod(rc, 0o644)
	removePathFromShellConfigs(context.Background(), io.Discard)
	st, _ := os.Stat(rc)
	if mode := st.Mode().Perm(); mode != 0o644 {
		t.Fatalf("mode changed to %o", mode)
	}
}

// Fake home with the installer's node, npm and npx under the icebath home
func uninstallMakeNode(t *testing.T, h string) string {
	t.Helper()
	nodeBin := filepath.Join(h, "node", "bin")
	for _, name := range []string{"node", "npm", "npx"} {
		uninstallWrite(t, filepath.Join(nodeBin, name), "#!/bin/sh\n")
		os.Chmod(filepath.Join(nodeBin, name), 0o755)
	}
	return nodeBin
}

func TestNodeLinkCleanupRespectsResolvedOwner(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "False", true: "True"}[owned], func(t *testing.T) {
			fake := uninstallFakeHome(t)
			h := filepath.Join(fake, ".icebath")
			link := filepath.Join(fake, ".local", "bin", "node")
			target := filepath.Join(fake, "other", "node")
			if owned {
				target = filepath.Join(h, "node", "bin", "node")
			}
			uninstallWrite(t, target, "retained binary")
			os.MkdirAll(filepath.Dir(link), 0o755)
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("native symlink creation unavailable: %v", err)
			}
			swap(t, &uninstallNodeSymlinkDirs, func() []string { return []string{filepath.Dir(link)} })
			removed := removeNodeSymlinks(io.Discard, h)
			want := []string(nil)
			if owned {
				want = []string{link}
			}
			if !slices.Equal(removed, want) {
				t.Fatalf("removed %v", removed)
			}
			if _, err := os.Stat(link); (err == nil) != !owned {
				t.Fatalf("link exists %v", err == nil)
			}
			if text, _ := os.ReadFile(target); string(text) != "retained binary" {
				t.Fatalf("target %q", text)
			}
		})
	}
}

func TestLeavesUnrelatedSymlinksUntouched(t *testing.T) {
	fake := uninstallFakeHome(t)
	h := filepath.Join(fake, ".icebath")
	uninstallMakeNode(t, h)
	localBin := filepath.Join(fake, ".local", "bin")
	nvmNode := filepath.Join(fake, ".nvm", "versions", "node", "v20.0.0", "bin", "node")
	uninstallWrite(t, nvmNode, "#!/bin/sh\n")
	os.MkdirAll(localBin, 0o755)
	if err := os.Symlink(nvmNode, filepath.Join(localBin, "node")); err != nil {
		t.Skipf("native symlink creation unavailable: %v", err)
	}
	swap(t, &uninstallNodeSymlinkDirs, func() []string { return []string{localBin} })
	if removed := removeNodeSymlinks(io.Discard, h); len(removed) != 0 {
		t.Fatalf("removed %v", removed)
	}
	if st, err := os.Lstat(filepath.Join(localBin, "node")); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("nvm link removed")
	}
	got, _ := filepath.EvalSymlinks(filepath.Join(localBin, "node"))
	want, _ := filepath.EvalSymlinks(nvmNode)
	if got != want {
		t.Fatalf("resolves to %s", got)
	}
}

func TestRemovesFhsSymlinksInUsrLocalBin(t *testing.T) {
	fake := uninstallFakeHome(t)
	h := filepath.Join(fake, ".icebath")
	nodeBin := uninstallMakeNode(t, h)
	fhsBin := filepath.Join(filepath.Dir(fake), "usr_local_bin")
	os.MkdirAll(fhsBin, 0o755)
	for _, name := range []string{"node", "npm", "npx"} {
		if err := os.Symlink(filepath.Join(nodeBin, name), filepath.Join(fhsBin, name)); err != nil {
			t.Skipf("native symlink creation unavailable: %v", err)
		}
	}
	swap(t, &uninstallNodeSymlinkDirs, func() []string { return []string{fhsBin} })
	removed := removeNodeSymlinks(io.Discard, h)
	var names []string
	for _, p := range removed {
		names = append(names, filepath.Base(p))
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"node", "npm", "npx"}) {
		t.Fatalf("removed %v", names)
	}
	for _, name := range []string{"node", "npm", "npx"} {
		if uninstallExists(filepath.Join(fhsBin, name)) {
			t.Fatalf("%s survived", name)
		}
	}
}

// Default root bin holding staged launcher copies
func uninstallManagedBin(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), "icebath")
	binDir := filepath.Join(h, "bin")
	os.MkdirAll(binDir, 0o755)
	os.WriteFile(filepath.Join(binDir, "icebath.exe"), []byte("MZ launcher"), 0o755)
	os.WriteFile(filepath.Join(binDir, "icebath-acp.cmd"), []byte("@echo off\r\n"), 0o644)
	t.Setenv("ICEBATH_HOME", h)
	return binDir
}

func uninstallBaseNames(paths []string) []string {
	var out []string
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	slices.Sort(out)
	return out
}

func TestRemovesTheWholeManagedBinDir(t *testing.T) {
	binDir := uninstallManagedBin(t)
	removed := removeWindowsBinLaunchers(io.Discard, true)
	if got := uninstallBaseNames(removed); !slices.Equal(got, []string{"icebath-acp.cmd", "icebath.exe"}) {
		t.Fatalf("removed %v", got)
	}
	if uninstallExists(binDir) {
		t.Fatal("bin dir survived")
	}
}

func TestAnchorsOnDefaultRootNotProfileHome(t *testing.T) {
	binDir := uninstallManagedBin(t)
	t.Setenv("ICEBATH_HOME", filepath.Join(filepath.Dir(binDir), "profiles", "work"))
	removed := removeWindowsBinLaunchers(io.Discard, true)
	if got := uninstallBaseNames(removed); !slices.Equal(got, []string{"icebath-acp.cmd", "icebath.exe"}) {
		t.Fatalf("removed %v", got)
	}
	if uninstallExists(filepath.Join(binDir, "icebath.exe")) {
		t.Fatal("launcher survived")
	}
}

func TestNoopOnPosix(t *testing.T) {
	binDir := uninstallManagedBin(t)
	if removed := removeWindowsBinLaunchers(io.Discard, false); len(removed) != 0 {
		t.Fatalf("removed %v", removed)
	}
	if !uninstallExists(filepath.Join(binDir, "icebath.exe")) {
		t.Fatal("launcher removed")
	}
}

func TestNoopWhenNoBinDir(t *testing.T) {
	h := filepath.Join(t.TempDir(), "icebath")
	os.MkdirAll(h, 0o755)
	t.Setenv("ICEBATH_HOME", h)
	if removed := removeWindowsBinLaunchers(io.Discard, true); len(removed) != 0 {
		t.Fatalf("removed %v", removed)
	}
}

func TestLegacyCleanupRemovesOnlyRuntimeBytesAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	runtimeFiles := []string{"node/bin/node", "bin/uv", "bin/uv.exe"}
	user := []string{"bin/my-script", "config.yaml", "auth.json", "SOUL.md", "sessions/session", "skills/demo/SKILL.md",
		"memories/MEMORY.md", "profiles/other/config.yaml"}
	for _, name := range append(slices.Clone(runtimeFiles), user...) {
		uninstallWrite(t, filepath.Join(root, filepath.FromSlash(name)), name)
	}
	removed := removeLegacyRuntimeTrees(io.Discard, root)
	want := []string{filepath.Join(root, "node"), filepath.Join(root, "bin", "uv"), filepath.Join(root, "bin", "uv.exe")}
	slices.Sort(removed)
	slices.Sort(want)
	if !slices.Equal(removed, want) {
		t.Fatalf("removed %v", removed)
	}
	for _, name := range runtimeFiles {
		if uninstallExists(filepath.Join(root, filepath.FromSlash(name))) {
			t.Fatalf("%s survived", name)
		}
	}
	for _, name := range user {
		if text, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(name))); string(text) != name {
			t.Fatalf("%s changed", name)
		}
	}
	if again := removeLegacyRuntimeTrees(io.Discard, root); len(again) != 0 {
		t.Fatalf("second pass %v", again)
	}
}

// LaunchAgents dir under the fake home that the gateway plist lookup answers with
func uninstallLaunchAgents(t *testing.T) string {
	t.Helper()
	fake := uninstallFakeHome(t)
	agents := filepath.Join(fake, "Library", "LaunchAgents")
	os.MkdirAll(agents, 0o755)
	swap(t, &uninstallLaunchdGatewayPlist, func(context.Context) (string, error) {
		return filepath.Join(agents, "ai.icebath.gateway.plist"), nil
	})
	return agents
}

// Records every command the uninstall would run instead of running it
func uninstallFakeRun(t *testing.T) *[][]string {
	t.Helper()
	calls := &[][]string{}
	swap(t, &uninstallRunCommand, func(argv []string, _ time.Duration) error {
		*calls = append(*calls, slices.Clone(argv))
		return nil
	})
	return calls
}

func TestRemoveLaunchdGatewaySweepsBothLabelPatterns(t *testing.T) {
	agents := uninstallLaunchAgents(t)
	current := filepath.Join(agents, "ai.icebath.gateway.plist")
	profiled := filepath.Join(agents, "ai.icebath.gateway-work.plist")
	legacy := filepath.Join(agents, "io.nousresearch.icebath-agent.gateway.plist")
	unrelated := filepath.Join(agents, "com.other.agent.plist")
	for _, p := range []string{current, profiled, legacy, unrelated} {
		uninstallWrite(t, p, "<plist/>")
	}
	calls := uninstallFakeRun(t)
	removed, err := removeLaunchdGateway(context.Background(), io.Discard)
	if err != nil || !removed {
		t.Fatalf("removed %v %v", removed, err)
	}
	if uninstallExists(current) || uninstallExists(profiled) || uninstallExists(legacy) || !uninstallExists(unrelated) {
		t.Fatal("wrong plists removed")
	}
	labels := map[string]bool{}
	for _, c := range *calls {
		if c[1] == "bootout" {
			labels[c[len(c)-1][strings.LastIndex(c[len(c)-1], "/")+1:]] = true
		}
	}
	want := map[string]bool{"ai.icebath.gateway": true, "ai.icebath.gateway-work": true, "io.nousresearch.icebath-agent.gateway": true}
	if len(labels) != len(want) {
		t.Fatalf("labels %v", labels)
	}
	for l := range want {
		if !labels[l] {
			t.Fatalf("labels %v", labels)
		}
	}
	for _, c := range *calls {
		if c[1] != "bootout" && c[1] != "unload" {
			t.Fatalf("call %v", c)
		}
	}
}

func TestRemoveLaunchdGatewayReturnsFalseWhenNoPlists(t *testing.T) {
	uninstallLaunchAgents(t)
	calls := uninstallFakeRun(t)
	removed, err := removeLaunchdGateway(context.Background(), io.Discard)
	if err != nil || removed || len(*calls) != 0 {
		t.Fatalf("removed %v %v calls %v", removed, err, *calls)
	}
}

func TestRemoveDesktopAppLeftoversRemovesHermesLibraryEntries(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Library layout")
	}
	fake := uninstallFakeHome(t)
	library := filepath.Join(fake, "Library")
	var ours []string
	for _, pair := range [][2]string{{"Caches", "com.nousresearch.icebath"}, {"Logs", "Icebath"}, {"WebKit", "com.nousresearch.icebath"},
		{"HTTPStorages", "com.nousresearch.icebath"}, {"Saved Application State", "com.nousresearch.icebath.savedState"}} {
		d := filepath.Join(library, pair[0], pair[1])
		os.MkdirAll(d, 0o755)
		ours = append(ours, d)
	}
	os.MkdirAll(filepath.Join(library, "Caches", "com.other.vendor"), 0o755)
	removed := removeDesktopAppLeftovers(io.Discard, false)
	for _, d := range ours {
		if uninstallExists(d) {
			t.Fatalf("%s survived", d)
		}
	}
	if !uninstallExists(filepath.Join(library, "Caches", "com.other.vendor")) || len(removed) != len(ours) {
		t.Fatalf("removed %v", removed)
	}
}

func TestXdgCacheRemovedAndDataModeSemantics(t *testing.T) {
	fake := uninstallFakeHome(t)
	cache := filepath.Join(fake, ".cache", "icebath")
	data := filepath.Join(fake, ".local", "share", "icebath")
	os.MkdirAll(cache, 0o755)
	os.MkdirAll(data, 0o755)
	if r, p := xdgLeftoverPaths(false); !slices.Equal(r, []string{cache}) || !slices.Equal(p, []string{data}) {
		t.Fatalf("keep data %v %v", r, p)
	}
	if r, p := xdgLeftoverPaths(true); !slices.Equal(r, []string{cache, data}) || len(p) != 0 {
		t.Fatalf("full %v %v", r, p)
	}
	removed := removeDesktopAppLeftovers(io.Discard, false)
	if !slices.Contains(removed, cache) || uninstallExists(cache) || !uninstallExists(data) {
		t.Fatalf("removed %v", removed)
	}
}

func TestXdgDataRemovedOnFullUninstall(t *testing.T) {
	fake := uninstallFakeHome(t)
	cache := filepath.Join(fake, ".cache", "icebath")
	data := filepath.Join(fake, ".local", "share", "icebath")
	os.MkdirAll(cache, 0o755)
	os.MkdirAll(data, 0o755)
	removed := removeDesktopAppLeftovers(io.Discard, true)
	if uninstallExists(cache) || uninstallExists(data) || !slices.Contains(removed, cache) || !slices.Contains(removed, data) {
		t.Fatalf("removed %v", removed)
	}
}

// Standalone gateway sweep that finds nothing and must not kill, with no Termux environment
func uninstallNoLiveGateways(t *testing.T) {
	t.Helper()
	swap(t, &uninstallFindGatewayPids, func(context.Context) ([]int, error) { return nil, nil })
	swap(t, &uninstallKillGatewayProcesses, func(context.Context, io.Writer) (int, error) {
		t.Error("kill_gateway_processes must not run without live pids")
		return 0, nil
	})
	t.Setenv("TERMUX_VERSION", "")
	os.Unsetenv("TERMUX_VERSION")
	t.Setenv("PREFIX", "")
}

// Remover table recording which platform's remover ran
func uninstallDispatchTable(calls *[]string) map[string]gatewayServiceRemover {
	remover := func(name string) func(context.Context, io.Writer) (bool, error) {
		return func(context.Context, io.Writer) (bool, error) {
			*calls = append(*calls, name)
			return true, nil
		}
	}
	return map[string]gatewayServiceRemover{"Linux": {remover("Linux"), "linux label"}, "Darwin": {remover("Darwin"), "darwin label"},
		"Windows": {remover("Windows"), "windows label"}}
}

func TestCurrentPlatformRemoverRunsAndOthersDoNot(t *testing.T) {
	uninstallNoLiveGateways(t)
	var calls []string
	swap(t, &uninstallGatewayServiceRemover, uninstallDispatchTable(&calls))
	if !uninstallGatewayService(context.Background(), io.Discard) {
		t.Fatal("service reported nothing stopped")
	}
	if !slices.Equal(calls, []string{pyplatform.System()}) {
		t.Fatalf("calls %v", calls)
	}
}

func TestRemoverFailureReportsTheTableLabel(t *testing.T) {
	uninstallNoLiveGateways(t)
	table := uninstallDispatchTable(&[]string{})
	table[pyplatform.System()] = gatewayServiceRemover{func(context.Context, io.Writer) (bool, error) {
		return false, errors.New("no service manager here")
	}, "remover blew up"}
	swap(t, &uninstallGatewayServiceRemover, table)
	var out bytes.Buffer
	if uninstallGatewayService(context.Background(), &out) {
		t.Fatal("failed remover reported success")
	}
	if !strings.Contains(out.String(), "remover blew up") {
		t.Fatalf("output %q", out.String())
	}
}

func TestUnknownPlatformRunsNoRemover(t *testing.T) {
	uninstallNoLiveGateways(t)
	swap(t, &uninstallGatewayServiceRemover, map[string]gatewayServiceRemover{})
	if uninstallGatewayService(context.Background(), io.Discard) {
		t.Fatal("empty table reported success")
	}
}

func TestUninstallArgsNamespaceModeMapping(t *testing.T) {
	gui := uninstallModeNamespace("gui", nil)
	if gui["gui"] != true || gui["full"] != false || gui["yes"] != true {
		t.Fatalf("gui %v", gui)
	}
	lite := uninstallModeNamespace("lite", nil)
	if lite["gui"] != false || lite["full"] != false || lite["yes"] != true {
		t.Fatalf("lite %v", lite)
	}
	full := uninstallModeNamespace("full", nil)
	if full["gui"] != false || full["full"] != true || full["yes"] != true {
		t.Fatalf("full %v", full)
	}
}

func TestModuleEntrypointRoutesDataMode(t *testing.T) {
	type call struct {
		mode string
		ns   map[string]any
	}
	var calls []call
	record := func(mode string) func(context.Context, map[string]any, io.Writer) (int, error) {
		return func(_ context.Context, ns map[string]any, _ io.Writer) (int, error) {
			calls = append(calls, call{mode, ns})
			return 0, nil
		}
	}
	swap(t, &uninstallRunDataMode, record("data"))
	swap(t, &uninstallRunGUIMode, record("gui"))
	swap(t, &uninstallRunAgentMode, record("agent"))
	for _, step := range [][2]string{{"data", "data"}, {"gui", "gui"}, {"full", "agent"}} {
		if code := UninstallModuleMain(context.Background(), []string{"--mode", step[0]}, io.Discard, io.Discard); code != 0 {
			t.Fatalf("%s exit %d", step[0], code)
		}
		if calls[len(calls)-1].mode != step[1] {
			t.Fatalf("%s routed to %s", step[0], calls[len(calls)-1].mode)
		}
	}
	if calls[len(calls)-1].ns["full"] != true {
		t.Fatalf("full namespace %v", calls[len(calls)-1].ns)
	}
}
