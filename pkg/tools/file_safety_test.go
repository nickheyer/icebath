package tools

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestIsWriteDeniedAndApprovalExports(t *testing.T) {
	env := &Env{Home: t.TempDir()}
	for _, p := range []string{"/etc/shadow", filepath.Join(env.Home, ".env"), filepath.Join(t.TempDir(), "note.mp3")} {
		if got, want := IsWriteDenied(env, p), classifyWriteDenial(env, p, false) != ""; got != want {
			t.Fatalf("IsWriteDenied(%q) = %v, want %v", p, got, want)
		}
		if got, want := IsWriteApprovalRequired(env, p), isWriteApprovalRequired(env, p); got != want {
			t.Fatalf("IsWriteApprovalRequired(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestExportedRedactionHelpersMatchTheirPrivateForms(t *testing.T) {
	if CanonicalURLParamName("Api%5Fkey") != canonicalURLParamName("Api%5Fkey") || CanonicalURLParamName("X-Amz-Signature") != "x-amz-signature" {
		t.Fatal("CanonicalURLParamName")
	}
	names := SensitiveQueryParamNames()
	if len(names) != len(sensitiveQueryParams) || !slices.IsSorted(names) || !slices.Contains(names, "api_key") {
		t.Fatalf("SensitiveQueryParamNames %v", names)
	}
	if MaskDisplaySecret("sk-proj-1234567890", 4, 4, 12) != maskSecret("sk-proj-1234567890", 4, 4, 12) || MaskDisplaySecret("short", 4, 4, 12) != "***" || MaskDisplaySecret("", 4, 4, 12) != "" {
		t.Fatal("MaskDisplaySecret")
	}
}

func TestFileSafetyExportsReadTheGuardTables(t *testing.T) {
	h := t.TempDir()
	dirs := IcebathDirs(&Env{Home: h})
	if len(dirs) == 0 || dirs[0] != resolveNonStrict(h) {
		t.Fatalf("dirs %v", dirs)
	}
	if !slices.Contains(HomeCredentialDirs(), ".ssh") || !slices.Contains(HomeCredentialDirs(), ".config/gcloud") {
		t.Fatal(HomeCredentialDirs())
	}
	if !IsNTNamespacePath(`\??\C:\x`) || !IsNTNamespacePath(`\\?\UNC\server\share`) || IsNTNamespacePath("/tmp/x") {
		t.Fatal("namespace check must follow the raw string rule")
	}
}
