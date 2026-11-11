package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Default certificate name of the self signed TCC signing identity
const defaultTCCIdentity = "Icebath Local Signing"

// True when the identity is among the valid code signing identities, never failing
func macosCodesigningIdentityValid(security, identity string) bool {
	res, err := gui.capture([]string{security, "find-identity", "-v", "-p", "codesigning"}, nil)
	if err != nil {
		return false
	}
	return strings.Contains(pystr.DecodeUTF8Replace(res.Stdout), "\""+identity+"\"")
}

// Runs a capture with check semantics, a non zero exit raising CalledProcessError
func tccCheckedRun(argv []string) error {
	res, err := gui.capture(argv, nil)
	if err != nil {
		return err
	}
	if res.Returncode != 0 {
		return &pysubprocess.CalledProcessError{Cmd: argv, Returncode: res.Returncode, Stdout: res.Stdout, Stderr: res.Stderr}
	}
	return nil
}

// Creates a ten year self signed code signing certificate, imports it for codesign and trusts it
func macosCreateSigningIdentity(w io.Writer, openssl, security, codesign, keychain, identity string) (bool, error) {
	tmpDir, err := os.MkdirTemp("", "icebath-tcc-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmpDir)
	key := filepath.Join(tmpDir, "sign.key")
	crt := filepath.Join(tmpDir, "sign.crt")
	p12 := filepath.Join(tmpDir, "sign.p12")
	if err := tccCheckedRun([]string{openssl, "req", "-x509", "-newkey", "rsa:2048",
		"-keyout", key, "-out", crt, "-days", "3650", "-nodes", "-subj", "/CN=" + identity,
		"-addext", "basicConstraints=critical,CA:TRUE",
		"-addext", "keyUsage=critical,digitalSignature,keyCertSign",
		"-addext", "extendedKeyUsage=codeSigning"}); err != nil {
		fmt.Fprintf(w, "  (certificate creation failed: %s)\n", tccErrText(err))
		return false, nil
	}
	exportP12 := func(extra []string) error {
		argv := append([]string{openssl, "pkcs12", "-export"}, extra...)
		return tccCheckedRun(append(argv, "-inkey", key, "-in", crt, "-out", p12, "-passout", "pass:icebathlocal"))
	}
	importP12 := func() (*pysubprocess.Completed, error) {
		return gui.capture([]string{security, "import", p12, "-k", keychain, "-P", "icebathlocal",
			"-T", codesign, "-T", "/usr/bin/codesign_allocate"}, nil)
	}
	if err := exportP12(nil); err != nil {
		fmt.Fprintf(w, "  (certificate creation failed: %s)\n", tccErrText(err))
		return false, nil
	}
	imported, err := importP12()
	if err != nil {
		fmt.Fprintf(w, "  (certificate creation failed: %s)\n", tccErrText(err))
		return false, nil
	}
	if imported.Returncode != 0 && strings.Contains(pystr.DecodeUTF8Replace(imported.Stderr), "MAC verification failed") {
		legacyErr := exportP12([]string{"-legacy"})
		var called *pysubprocess.CalledProcessError
		if legacyErr == nil {
			if imported, err = importP12(); err != nil {
				fmt.Fprintf(w, "  (certificate creation failed: %s)\n", tccErrText(err))
				return false, nil
			}
		} else if !errors.As(legacyErr, &called) {
			fmt.Fprintf(w, "  (certificate creation failed: %s)\n", tccErrText(legacyErr))
			return false, nil
		}
	}
	if imported.Returncode != 0 {
		fmt.Fprintf(w, "  (could not import signing identity into keychain: %s)\n", pystr.Strip(pystr.DecodeUTF8Replace(imported.Stderr)))
		return false, nil
	}
	trusted, err := gui.capture([]string{security, "add-trusted-cert", "-r", "trustRoot", "-p", "codeSign", "-k", keychain, crt}, nil)
	if err != nil {
		fmt.Fprintf(w, "  (certificate creation failed: %s)\n", tccErrText(err))
		return false, nil
	}
	if trusted.Returncode != 0 {
		detail := pystr.DecodeUTF8Replace(trusted.Stderr)
		if detail == "" {
			detail = pystr.DecodeUTF8Replace(trusted.Stdout)
		}
		fmt.Fprintf(w, "  (could not trust the certificate for code signing: %s)\n", pystr.Strip(detail))
		return false, nil
	}
	fmt.Fprintf(w, "  → created, imported, and trusted self-signed identity: %s\n", pyrepr.StrRepr(identity))
	return true, nil
}

// Python str of an exception raised while creating the certificate
func tccErrText(err error) string {
	var called *pysubprocess.CalledProcessError
	if errors.As(err, &called) {
		return called.Error()
	}
	return guiOSErrorText(err)
}

// Creates or reuses a self signed code signing identity, configures it and re-signs the packaged app
func desktopMacosSetupTCCIdentity(ctx context.Context, w io.Writer, identity string) (bool, error) {
	if gui.platform() != "darwin" {
		fmt.Fprintln(w, "  (--setup-tcc-identity is macOS-only; skipping)")
		return false, nil
	}
	openssl, security, codesign := gui.which("openssl"), gui.which("security"), gui.which("codesign")
	if openssl == "" || security == "" || codesign == "" {
		fmt.Fprintf(w, "  (--setup-tcc-identity requires openssl, security, and codesign; found openssl=%s security=%s codesign=%s)\n",
			pyBool(openssl != ""), pyBool(security != ""), pyBool(codesign != ""))
		return false, nil
	}
	keychain := filepath.Join(gui.homeDir(), "Library", "Keychains", "login.keychain-db")
	if macosCodesigningIdentityValid(security, identity) {
		fmt.Fprintf(w, "  → identity %s already valid in keychain\n", pyrepr.StrRepr(identity))
	} else if created, err := macosCreateSigningIdentity(w, openssl, security, codesign, keychain, identity); err != nil || !created {
		return false, err
	}
	if !macosCodesigningIdentityValid(security, identity) {
		fmt.Fprintf(w, "  (identity %s was imported but is not a VALID code-signing identity; "+
			"run `security find-identity -v -p codesigning` to inspect, and see the manual "+
			"Keychain Access steps in the desktop docs)\n", pyrepr.StrRepr(identity))
		return false, nil
	}
	if err := gui.setConfigValue(ctx, "desktop.macos_signing_identity", identity); err != nil {
		if code, ok := guiSystemExit(err); ok {
			return false, &guiExit{code: code}
		}
		fmt.Fprintf(w, "  (could not write desktop.macos_signing_identity: %s)\n", err.Error())
		return false, nil
	}
	fmt.Fprintf(w, "  → set desktop.macos_signing_identity = %s\n", pyrepr.StrRepr(identity))
	desktopDir := filepath.Join(gui.projectRoot(), "apps", "desktop")
	if _, ok := gui.packagedExecutable(desktopDir); ok {
		if gui.relaunchableFixup(desktopDir) {
			fmt.Fprintln(w, "  → packaged app re-signed with certificate-anchored identity; TCC grants persist across rebuilds")
		}
	}
	fmt.Fprintln(w, "\n  Note: macOS will re-prompt for permissions ONE final time (the identity "+
		"changed). Grant them and they persist from then on. If a permission gets "+
		"stuck, reset it with:  tccutil reset All com.nousresearch.icebath")
	return true, nil
}
