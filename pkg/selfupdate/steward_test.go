package selfupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Writes an install stamp with a commit and update mechanism plus the given fields
func stStamp(t *testing.T, root, mechanism string, fields map[string]any) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"commit": strings.Repeat("a", 40), "updateMechanism": mechanism}
	for key, value := range fields {
		payload[key] = value
	}
	data, _ := json.Marshal(payload)
	if err := os.WriteFile(filepath.Join(root, "install-stamp.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Replaces the package database query with one owning nothing
func stNoOwner(t *testing.T) {
	previous := packageOwner
	packageOwner = func(string) string { return "" }
	t.Cleanup(func() { packageOwner = previous })
}

func stSteward(steward *string) string {
	if steward == nil {
		return "<nil>"
	}
	return *steward
}

func TestStewardSealedTreeNamesItsSteward(t *testing.T) {
	stNoOwner(t)
	for _, tc := range [][2]string{{StewardNix, "external"}, {StewardDocker, "external"}, {StewardDesktop, "electron-updater"}} {
		root := filepath.Join(t.TempDir(), "install")
		stStamp(t, root, tc[1], map[string]any{"distribution": tc[0]})
		if got := stSteward(SealedSteward(root)); got != tc[0] {
			t.Fatalf("%s got %s", tc[0], got)
		}
		if steward, allowed := ClassifyInstall(root); steward != tc[0] || allowed {
			t.Fatalf("classify %s %v", steward, allowed)
		}
	}
}

func TestStewardGitCheckoutIsNotStewardOwned(t *testing.T) {
	root := filepath.Join(t.TempDir(), "checkout")
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	if SealedSteward(root) != nil {
		t.Fatal("checkout sealed")
	}
	if steward, allowed := ClassifyInstall(root); steward != "git" || !allowed {
		t.Fatalf("classify %s %v", steward, allowed)
	}
}

func TestStewardGitBeatsStamp(t *testing.T) {
	root := filepath.Join(t.TempDir(), "checkout")
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	stStamp(t, root, "self", map[string]any{"distribution": nil})
	if SealedSteward(root) != nil {
		t.Fatal("checkout sealed")
	}
}

func TestStewardWorktreeGitfileCountsAsCheckout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktree")
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: ../elsewhere\n"), 0o644)
	if SealedSteward(root) != nil {
		t.Fatal("worktree sealed")
	}
}

func TestStewardMissingStampGivesUnknownSteward(t *testing.T) {
	stNoOwner(t)
	root := filepath.Join(t.TempDir(), "bare")
	os.MkdirAll(root, 0o755)
	if got := stSteward(SealedSteward(root)); got != "unknown" {
		t.Fatal(got)
	}
	if steward, allowed := ClassifyInstall(root); steward != "unknown" || allowed {
		t.Fatalf("classify %s %v", steward, allowed)
	}
}

func TestStewardExecutingNixTreeReadsTheStampTheWrapperPointsAt(t *testing.T) {
	stNoOwner(t)
	share := filepath.Join(t.TempDir(), "store", "share", "icebath-agent")
	stStamp(t, share, "external", map[string]any{"distribution": "nix"})
	t.Setenv(pm.EnvInstallRoot, share)
	if got := stSteward(SealedSteward(pm.RepoRoot())); got != "nix" {
		t.Fatal(got)
	}
	if got := pm.InstallStampPath(pm.RepoRoot()); got != filepath.Join(share, "install-stamp.json") {
		t.Fatal(got)
	}
	other := filepath.Join(t.TempDir(), "elsewhere")
	os.MkdirAll(other, 0o755)
	if got := stSteward(SealedSteward(other)); got != "unknown" {
		t.Fatal(got)
	}
}

func TestStewardMalformedStampDegradesToUnknown(t *testing.T) {
	stNoOwner(t)
	root := filepath.Join(t.TempDir(), "install")
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(root, "install-stamp.json"), []byte("{not json"), 0o644)
	if stamp := pm.ReadInstallStamp(root); stamp.Len() != 0 {
		t.Fatalf("stamp %v", stamp)
	}
	if got := stSteward(SealedSteward(root)); got != "unknown" {
		t.Fatal(got)
	}
}

func TestStewardPackageManagerLayoutsAndOwnership(t *testing.T) {
	t.Setenv("SCOOP", "")
	t.Setenv("SCOOP_GLOBAL", "")
	for path, want := range map[string]string{
		"/nix/store/abc-icebath-1.0/bin/icebath":                                      StewardNix,
		"/opt/homebrew/Caskroom/icebath/1.0/icebath":                                  StewardHomebrew,
		"/usr/local/Cellar/icebath/1.0/bin/icebath":                                   StewardHomebrew,
		`C:\Users\me\scoop\apps\icebath\current\icebath.exe`:                          StewardScoop,
		`C:\Users\me\AppData\Local\Microsoft\WinGet\Packages\nickheyer.icebath\x.exe`: StewardWinGet,
		"/home/me/.local/bin/icebath":                                                 "",
		"/home/me/nix/store/icebath":                                                  "",
	} {
		if got := packageManagerPath(strings.ReplaceAll(path, `\`, "/")); got != want {
			t.Fatalf("%s got %q want %q", path, got, want)
		}
	}
	scoop := filepath.Join(t.TempDir(), "custom-scoop")
	t.Setenv("SCOOP", scoop)
	if got := packageManagerPath(filepath.Join(scoop, "apps", "icebath", "current", "icebath")); got != StewardScoop {
		t.Fatal(got)
	}
	previous := packageOwner
	packageOwner = func(binary string) string {
		if binary == installBinary(filepath.Join("/", "usr", "bin")) {
			return StewardDpkg
		}
		return ""
	}
	t.Cleanup(func() { packageOwner = previous })
	if got := stSteward(SealedSteward(filepath.Join("/", "usr", "bin"))); got != StewardDpkg {
		t.Fatal(got)
	}
	if !strings.Contains(StewardUpdateMessage(StewardDpkg), "apt install --only-upgrade icebath") {
		t.Fatal(StewardUpdateMessage(StewardDpkg))
	}
}

func TestStewardMessages(t *testing.T) {
	if got := StewardUpdateMessage("flatpak"); got != "✗ This icebath install is managed by flatpak.\n\nThe tree has no git checkout, so `icebath update` cannot update it.\nUpdate it with the tool that installed it." {
		t.Fatal(got)
	}
	if !strings.Contains(StewardUpdateMessage(StewardNix), "Nix store") || !strings.Contains(StewardUpdateMessage(StewardDesktop), "desktop app") {
		t.Fatal("steward texts")
	}
	win, mac, linux := "win32", "darwin", "linux"
	if !strings.Contains(StewardUninstallMessage(StewardDesktop, &win), "Windows Settings") ||
		!strings.Contains(StewardUninstallMessage(StewardDesktop, &mac), "icebath.app") ||
		StewardUninstallMessage(StewardDesktop, &linux) != stewardDesktopUninstallDefault {
		t.Fatal("desktop uninstall texts")
	}
	if got := StewardUninstallMessage("flatpak", nil); !strings.HasPrefix(got, "✗ icebath is managed by flatpak.\n") || !strings.HasSuffix(got, "run:\n$ icebath uninstall --data\n") {
		t.Fatal(got)
	}
	if !strings.Contains(StewardUninstallMessage(StewardDocker, nil), "docker rm <container>") {
		t.Fatal("docker uninstall text")
	}
}
