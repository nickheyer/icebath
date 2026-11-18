package pm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseNixPackagePinsTheRuntimeRequirementWheels(t *testing.T) {
	config, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	reqs, err := RuntimeRequirements()
	if err != nil {
		t.Fatal(err)
	}
	for system, suffix := range map[string]string{
		"x86_64-linux":   "-manylinux_2_17_x86_64.manylinux2014_x86_64.whl",
		"aarch64-linux":  "-manylinux_2_17_aarch64.manylinux2014_aarch64.whl",
		"aarch64-darwin": "-macosx_11_0_arm64.whl",
	} {
		found := false
		for _, f := range reqs[0].Files {
			if strings.HasSuffix(f.URL, suffix) {
				found = true
				if pin := system + ` = { url = "` + f.URL + `"; sha256 = "` + f.SHA256 + `"; };`; !strings.Contains(string(config), pin) {
					t.Fatalf(".goreleaser.yaml lacks %s", pin)
				}
			}
		}
		if !found {
			t.Fatalf("runtime requirements have no %s wheel", system)
		}
	}
}
