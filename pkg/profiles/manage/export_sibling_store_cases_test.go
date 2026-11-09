package manage

import (
	"path/filepath"
	"testing"
)

func TestCustomHermesHomeInsideACheckoutUsesASiblingStore(t *testing.T) {
	tmp := t.TempDir()
	checkout := filepath.Join(tmp, "checkout")
	gitFileCheckout(t, checkout)
	t.Chdir(checkout)
	exportHomes(t, checkout, filepath.Join(tmp, "home"))
	result, err := GetProfileExportPath("default", "20260823-120000")
	if err != nil {
		t.Fatal(err)
	}
	assertOutside(t, result, checkout)
}
