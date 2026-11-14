package app

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Faked macOS host with the signing tools present, every capture answered by answer and recorded
func tccFixture(t *testing.T, answer func(argv []string) *pysubprocess.Completed) (*[][]string, string) {
	t.Helper()
	swapGUISteps(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	gui.platform = func() string { return "darwin" }
	gui.which = func(name string) string {
		return map[string]string{"openssl": "/usr/bin/openssl", "security": "/usr/bin/security", "codesign": "/usr/bin/codesign"}[name]
	}
	gui.homeDir = func() string { return tmp }
	calls := &[][]string{}
	gui.capture = func(argv []string, _ []byte) (*pysubprocess.Completed, error) {
		*calls = append(*calls, append([]string{}, argv...))
		return answer(argv), nil
	}
	gui.packagedExecutable = func(string) (string, bool) { return "", false }
	gui.relaunchableFixup = func(string) bool { return true }
	gui.setConfigValue = func(context.Context, string, string) error { return nil }
	return calls, tmp
}

// Completed capture with the exit status and streams
func fakeProc(argv []string, code int, stdout, stderr string) *pysubprocess.Completed {
	return &pysubprocess.Completed{Args: argv, Returncode: code, Stdout: []byte(stdout), Stderr: []byte(stderr)}
}

// Whether argv starts with the security find identity listing
func isFindIdentity(argv []string) bool {
	return len(argv) >= 4 && slices.Equal(argv[:4], []string{"/usr/bin/security", "find-identity", "-v", "-p"})
}

// Runs the TCC identity setup and fails on an error
func setupTCC(t *testing.T, identity string) bool {
	t.Helper()
	ok, err := desktopMacosSetupTCCIdentity(context.Background(), &bytes.Buffer{}, identity)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// Answer of a find identity listing with or without the identity valid
func identityListing(argv []string, identity string, valid bool) *pysubprocess.Completed {
	if valid {
		return fakeProc(argv, 0, "  1) ABCD \""+identity+"\"\n     1 valid identities found", "")
	}
	return fakeProc(argv, 0, "     0 valid identities found", "")
}

// Whether any call starts with the tool and subcommand
func anyCall(calls [][]string, tool, sub string) bool {
	for _, c := range calls {
		if c[0] == tool && (sub == "" || slices.Contains(c, sub)) {
			return true
		}
	}
	return false
}

func TestSetupTccIdentityCreatesCertImportsTrustsAndConfigures(t *testing.T) {
	identity := "Icebath Local Signing"
	trusted := false
	calls, tmp := tccFixture(t, func(argv []string) *pysubprocess.Completed {
		if isFindIdentity(argv) {
			return identityListing(argv, identity, trusted)
		}
		if argv[0] == "/usr/bin/security" && argv[1] == "add-trusted-cert" {
			trusted = true
		}
		return fakeProc(argv, 0, "", "")
	})
	if !setupTCC(t, identity) {
		t.Fatal("setup failed")
	}
	if !anyCall(*calls, "/usr/bin/openssl", "req") || !anyCall(*calls, "/usr/bin/openssl", "pkcs12") ||
		!anyCall(*calls, "/usr/bin/security", "import") || !anyCall(*calls, "/usr/bin/security", "add-trusted-cert") {
		t.Fatalf("calls %q", *calls)
	}
	for _, c := range *calls {
		if c[1] == "add-trusted-cert" && (!slices.Contains(c, "codeSign") || !slices.Contains(c, "trustRoot")) {
			t.Fatalf("trust call %q", c)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "icebath-tcc-*")); len(left) != 0 {
		t.Fatalf("temp dirs left %v", left)
	}
}

func TestSetupTccIdentityRetriesPkcs12WithLegacyOnMacVerificationFailure(t *testing.T) {
	identity := "Icebath Local Signing"
	legacy, trusted := false, false
	calls, _ := tccFixture(t, func(argv []string) *pysubprocess.Completed {
		switch {
		case isFindIdentity(argv):
			return identityListing(argv, identity, trusted)
		case argv[0] == "/usr/bin/openssl" && slices.Contains(argv, "pkcs12"):
			legacy = slices.Contains(argv, "-legacy")
		case argv[0] == "/usr/bin/security" && argv[1] == "import" && !legacy:
			return fakeProc(argv, 1, "", "security: SecKeychainItemImport: MAC verification failed during PKCS12 import (wrong password?)")
		case argv[0] == "/usr/bin/security" && argv[1] == "add-trusted-cert":
			trusted = true
		}
		return fakeProc(argv, 0, "", "")
	})
	if !setupTCC(t, identity) {
		t.Fatal("setup failed")
	}
	var pkcs12, imports [][]string
	for _, c := range *calls {
		if c[0] == "/usr/bin/openssl" && slices.Contains(c, "pkcs12") {
			pkcs12 = append(pkcs12, c)
		}
		if c[0] == "/usr/bin/security" && c[1] == "import" {
			imports = append(imports, c)
		}
	}
	if len(pkcs12) != 2 || slices.Contains(pkcs12[0], "-legacy") || !slices.Contains(pkcs12[1], "-legacy") || len(imports) != 2 {
		t.Fatalf("pkcs12 %q imports %q", pkcs12, imports)
	}
}

func TestSetupTccIdentityFailsWhenTrustStepFails(t *testing.T) {
	tccFixture(t, func(argv []string) *pysubprocess.Completed {
		if isFindIdentity(argv) {
			return identityListing(argv, "", false)
		}
		if argv[0] == "/usr/bin/security" && argv[1] == "add-trusted-cert" {
			return fakeProc(argv, 1, "", "SecTrustSettingsSetTrustSettings: authorization denied")
		}
		return fakeProc(argv, 0, "", "")
	})
	if setupTCC(t, "Icebath Local Signing") {
		t.Fatal("setup succeeded")
	}
}

func TestSetupTccIdentityFailsWhenIdentityNeverBecomesValid(t *testing.T) {
	tccFixture(t, func(argv []string) *pysubprocess.Completed {
		if isFindIdentity(argv) {
			return identityListing(argv, "", false)
		}
		return fakeProc(argv, 0, "", "")
	})
	if setupTCC(t, "Icebath Local Signing") {
		t.Fatal("setup succeeded")
	}
}

func TestSetupTccIdentitySkipsGenerationWhenAlreadyValid(t *testing.T) {
	calls, _ := tccFixture(t, func(argv []string) *pysubprocess.Completed {
		if isFindIdentity(argv) {
			return identityListing(argv, "Icebath Local Signing", true)
		}
		return fakeProc(argv, 0, "", "")
	})
	if !setupTCC(t, "Icebath Local Signing") || anyCall(*calls, "/usr/bin/openssl", "") || anyCall(*calls, "/usr/bin/security", "import") {
		t.Fatalf("calls %q", *calls)
	}
}

func TestSetupTccIdentityUntrustedExistingCertIsRepaired(t *testing.T) {
	trusted := false
	calls, _ := tccFixture(t, func(argv []string) *pysubprocess.Completed {
		if isFindIdentity(argv) {
			return identityListing(argv, "Icebath Local Signing", trusted)
		}
		if argv[0] == "/usr/bin/security" && argv[1] == "add-trusted-cert" {
			trusted = true
		}
		return fakeProc(argv, 0, "", "")
	})
	if !setupTCC(t, "Icebath Local Signing") || !anyCall(*calls, "/usr/bin/security", "add-trusted-cert") {
		t.Fatalf("calls %q", *calls)
	}
}

func TestCmdGuiSetupTccIdentityExitsBeforeBuild(t *testing.T) {
	root := guiCheckout(t, t.TempDir())
	guiPackagedExe(t, root)
	guiLaunchFixture(t, root)
	var identities []string
	gui.setupTCCIdentity = func(_ context.Context, _ io.Writer, identity string) (bool, error) {
		identities = append(identities, identity)
		return true, nil
	}
	gui.prepare = func(context.Context, string, []string, map[string]string, bool) error {
		t.Fatal("dependencies installed")
		return nil
	}
	code, _ := runCmdGUI(t, guiNS(map[string]any{"setup_tcc_identity": true, "identity": "Icebath Local Signing"}), "")
	if code != 0 || !slices.Equal(identities, []string{"Icebath Local Signing"}) {
		t.Fatalf("code %d identities %v", code, identities)
	}
}
