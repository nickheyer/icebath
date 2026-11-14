package files

import (
	"path/filepath"
	"testing"
)

var installRoot = resolvePath("/opt/Hermes")

func TestIsPackagedInstallPathReturnsFalseWhenNotPackaged(t *testing.T) {
	if IsPackagedInstallPath(installRoot, []string{installRoot}, false) {
		t.Fatal("unpackaged flagged")
	}
}

func TestIsPackagedInstallPathFlagsTheInstallRootItself(t *testing.T) {
	if !IsPackagedInstallPath(installRoot, []string{installRoot}, true) {
		t.Fatal("root not flagged")
	}
}

func TestIsPackagedInstallPathFlagsPathsNestedUnderTheInstallRoot(t *testing.T) {
	if !IsPackagedInstallPath(filepath.Join(installRoot, "resources", "app.asar"), []string{installRoot}, true) {
		t.Fatal("nested not flagged")
	}
}

func TestIsPackagedInstallPathIgnoresPathsOutsideTheInstallRoot(t *testing.T) {
	if IsPackagedInstallPath(resolvePath("/home/user/projects/demo"), []string{installRoot}, true) {
		t.Fatal("outside flagged")
	}
}
