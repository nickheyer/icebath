package selfupdate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Points the baked marker path at path for one test
func ucMarkerPath(t *testing.T, path string) {
	t.Helper()
	saved := ImageProvenancePath
	ImageProvenancePath = path
	t.Cleanup(func() { ImageProvenancePath = saved })
}

// Makes the install method heuristic answer method for one test
func ucInstallMethod(t *testing.T, method string) {
	t.Helper()
	saved := admissionInstallMethod
	admissionInstallMethod = func(context.Context, string, string) (string, error) { return method, nil }
	t.Cleanup(func() { admissionInstallMethod = saved })
}

// Writes a valid image provenance marker and returns its path
func ucValidMarker(t *testing.T, dir string) string {
	t.Helper()
	marker := filepath.Join(dir, "image-provenance.json")
	data, _ := json.Marshal(map[string]any{"schema": 1, "deployment_kind": "image", "manager": "docker",
		"image": "nousresearch/hermes-agent", "version": "1.0.0", "revision": strings.Repeat("a", 40)})
	if err := os.WriteFile(marker, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return marker
}

// A sealed tree without a git dir stamped with a distribution
func ucSealedTree(t *testing.T, dir, distribution string) string {
	t.Helper()
	root := filepath.Join(dir, "sealed")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"distribution": distribution})
	if err := os.WriteFile(filepath.Join(root, "install-stamp.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// Runs the admission gate over root with an isolated home
func ucAdmission(t *testing.T, root string) *UpdateRefusal {
	t.Helper()
	if os.Getenv("ICEBATH_HOME") == "" || !strings.HasPrefix(os.Getenv("ICEBATH_HOME"), os.TempDir()) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
	}
	refused, err := evaluateUpdateAdmission(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return refused
}

func TestAdmissionMarkerRefusesEvenOnGitCheckout(t *testing.T) {
	tmp := t.TempDir()
	ucMarkerPath(t, ucValidMarker(t, tmp))
	ucInstallMethod(t, "git")
	refused := ucAdmission(t, tmp)
	if refused == nil || refused.Code != "image-marker" || !strings.Contains(refused.UpdateCommand, "docker pull") {
		t.Fatalf("refusal = %+v", refused)
	}
}

func TestAdmissionInvalidMarkerFailsClosed(t *testing.T) {
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "image-provenance.json")
	if err := os.WriteFile(bad, []byte("corrupted {{{"), 0o644); err != nil {
		t.Fatal(err)
	}
	ucMarkerPath(t, bad)
	refused := ucAdmission(t, tmp)
	if refused == nil || refused.Code != "image-marker-invalid" || !strings.Contains(refused.UpdateCommand, "docker pull") {
		t.Fatalf("refusal = %+v", refused)
	}
}

func TestAdmissionNoMarkerFallsBackToHeuristics(t *testing.T) {
	tmp := t.TempDir()
	ucMarkerPath(t, filepath.Join(tmp, "absent.json"))
	ucInstallMethod(t, "docker")
	if refused := ucAdmission(t, tmp); refused == nil || refused.Code != "docker" {
		t.Fatalf("refusal = %+v", refused)
	}
}

func TestAdmissionGitCheckoutNoMarkerIsAdmitted(t *testing.T) {
	tmp := t.TempDir()
	ucMarkerPath(t, filepath.Join(tmp, "absent.json"))
	ucInstallMethod(t, "git")
	if refused := ucAdmission(t, tmp); refused != nil {
		t.Fatalf("refusal = %+v", refused)
	}
}

func TestAdmissionNixRefuses(t *testing.T) {
	tmp := t.TempDir()
	ucMarkerPath(t, filepath.Join(tmp, "absent.json"))
	ucInstallMethod(t, "nix")
	if refused := ucAdmission(t, tmp); refused == nil || refused.Code != "nix" {
		t.Fatalf("refusal = %+v", refused)
	}
}

func TestRefusalReceiptWrittenAsRefused(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	installServeObligationHooks()
	ctx := updaterecord.UpdateReceiptScope(context.Background())
	recordRefusalReceipt(ctx, &UpdateRefusal{Code: "image-marker", Message: "msg", UpdateCommand: "docker pull nousresearch/hermes-agent:latest"})
	matches, err := filepath.Glob(filepath.Join(updaterecord.ReceiptDir(ctx), "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipts []string
	for _, match := range matches {
		if filepath.Base(match) != "latest.json" {
			receipts = append(receipts, match)
		}
	}
	if len(receipts) == 0 {
		t.Fatal("a refusal receipt must be written")
	}
	raw, err := os.ReadFile(receipts[0])
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Outcome    string `json:"outcome"`
		StopReason string `json:"stop_reason"`
		Steps      []struct {
			Name   string `json:"name"`
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if data.Outcome != "refused" || data.StopReason != "image-marker" {
		t.Fatalf("receipt = %+v", data)
	}
	found := false
	for _, step := range data.Steps {
		if step.Name == "admission" {
			found = true
			if step.OK || !strings.Contains(step.Detail, "docker pull") {
				t.Fatalf("admission step = %+v", step)
			}
		}
	}
	if !found {
		t.Fatalf("no admission step in %+v", data.Steps)
	}
}

func TestAdmissionSourceCheckoutOnTermuxHostRefusesWithAptHint(t *testing.T) {
	tmp := t.TempDir()
	ucMarkerPath(t, filepath.Join(tmp, "absent.json"))
	if err := os.Mkdir(filepath.Join(tmp, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ucUnsetenv(t, "TERMUX_VERSION")
	t.Setenv("PREFIX", "/data/data/com.termux/files/usr")
	refused := ucAdmission(t, tmp)
	if refused == nil || refused.Code != "apt-termux" {
		t.Fatalf("refusal = %+v", refused)
	}
	if !strings.Contains(refused.Message, "pkg install icebath-agent") || refused.UpdateCommand != "pkg install icebath-agent" {
		t.Fatalf("refusal = %+v", refused)
	}
	if strings.Contains(refused.UpdateCommand, "icebath update") {
		t.Fatalf("command = %q", refused.UpdateCommand)
	}
	t.Setenv("PREFIX", "/usr")
	if refused := ucAdmission(t, tmp); refused != nil {
		t.Fatalf("the same checkout off Termux stays updatable, got %+v", refused)
	}
}

func TestAdmissionAptTermuxRefusesWithPkgUpgrade(t *testing.T) {
	tmp := t.TempDir()
	ucMarkerPath(t, filepath.Join(tmp, "absent.json"))
	ucInstallMethod(t, "git")
	refused := ucAdmission(t, ucSealedTree(t, tmp, "apt-termux"))
	if refused == nil || refused.Code != "apt-termux" {
		t.Fatalf("refusal = %+v", refused)
	}
	if !strings.Contains(refused.Message, "pkg upgrade icebath-agent") || !strings.Contains(refused.UpdateCommand, "pkg upgrade icebath-agent") {
		t.Fatalf("refusal = %+v", refused)
	}
}

func TestAdmissionAptTermuxCommandComesFromStewardTable(t *testing.T) {
	tmp := t.TempDir()
	ucMarkerPath(t, filepath.Join(tmp, "absent.json"))
	ucInstallMethod(t, "git")
	t.Cleanup(config.PatchUpdateCommandForMethod("apt", "pkg upgrade icebath-agent --from-table"))
	refused := ucAdmission(t, ucSealedTree(t, tmp, "apt-termux"))
	if refused == nil || refused.Code != "apt-termux" {
		t.Fatalf("refusal = %+v", refused)
	}
	if refused.UpdateCommand != "pkg upgrade icebath-agent --from-table" {
		t.Fatalf("command = %q", refused.UpdateCommand)
	}
}

func TestAdmissionPackageManagerStampRefusesWithItsUpgradeCommand(t *testing.T) {
	for method, command := range map[string]string{
		"chocolatey": "choco upgrade icebath", "pip": "pipx upgrade icebath-cli", "homebrew": "brew upgrade --cask icebath",
		"scoop": "scoop update icebath", "winget": "winget upgrade --id nickheyer.icebath", "dpkg": "sudo apt install --only-upgrade icebath",
		"rpm": "sudo dnf upgrade icebath", "pacman": "paru -Syu icebath-bin", "apk": "apk upgrade icebath",
	} {
		tmp := t.TempDir()
		ucMarkerPath(t, filepath.Join(tmp, "absent.json"))
		if err := os.WriteFile(filepath.Join(tmp, ".install_method"), []byte(method+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		refused := ucAdmission(t, tmp)
		if refused == nil || refused.Code != method || !strings.Contains(refused.Message, command) || !strings.Contains(refused.UpdateCommand, command) {
			t.Fatalf("%s refusal = %+v", method, refused)
		}
		if refused.Message != StewardUpdateMessage(method) || strings.Contains(refused.Message, "{steward}") {
			t.Fatalf("%s message %q", method, refused.Message)
		}
	}
}

func TestAdmissionReleaseBinaryStampsAreAdmitted(t *testing.T) {
	for _, method := range []string{"script", "setup-app", "unknown"} {
		tmp := t.TempDir()
		ucMarkerPath(t, filepath.Join(tmp, "absent.json"))
		if err := os.WriteFile(filepath.Join(tmp, ".install_method"), []byte(method+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if refused := ucAdmission(t, tmp); refused != nil {
			t.Fatalf("%s refusal = %+v", method, refused)
		}
	}
}
