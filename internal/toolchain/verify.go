package toolchain

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/openpgp"
	pgperrors "golang.org/x/crypto/openpgp/errors"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/proc"
)

// Longest a smoke run of a freshly staged program may take
const probeTimeout = 60 * time.Second

// Longest one signature tool invocation may take
const signatureToolTimeout = 60 * time.Second

// Directory inside a staged tree that holds the release checksum list, its signature and the signing key
const provenanceDir = ".provenance"

// Paths below entry that pattern names, one plain path without a wildcard, a dangling link counted when links is set
func entryMatches(entry, pattern string, links bool) []string {
	full := filepath.Join(entry, filepath.FromSlash(pattern))
	if !strings.ContainsAny(pattern, "*?[") {
		if _, err := os.Stat(full); err != nil {
			if _, lerr := os.Lstat(full); !links || lerr != nil {
				return nil
			}
		}
		return []string{full}
	}
	matches, err := filepath.Glob(full)
	if err != nil {
		return nil
	}
	return matches
}

// Diagnosis for a required path that is not where the pin's layout puts it
func missingReason(entry, pattern string) string {
	return fmt.Sprintf("%s missing under %s; %s", pattern, entry, entryListing(entry))
}

// Empty when the install directory holds every entry point and file for target, else why not, running nothing
func (r *recipe) fileEvidence(entry string, target Target) string {
	if r.marker != "" {
		if fi, err := os.Stat(filepath.Join(entry, r.marker)); err != nil || !fi.Mode().IsRegular() {
			return missingReason(entry, r.marker)
		}
	}
	for _, pattern := range r.files {
		if len(entryMatches(entry, pattern, false)) == 0 {
			return missingReason(entry, pattern)
		}
	}
	binaries, pattern := r.binaries(entry, target)
	if pattern == "" && r.find == nil {
		if len(r.entry) == 0 {
			return ""
		}
		return "no entry point for this target"
	}
	if len(binaries) == 0 {
		if r.find != nil {
			return "executable missing under " + entry
		}
		return missingReason(entry, pattern)
	}
	for _, binary := range binaries {
		fi, err := os.Stat(binary)
		if target.bionic() {
			continue
		}
		if err != nil || !fi.Mode().IsRegular() {
			if r.kind == Program {
				return missingReason(entry, pattern)
			}
			continue
		}
		if machineMatches(binary, target) == archDiffers && !r.emulated[target] {
			return fmt.Sprintf("%s is not a %s binary", filepath.Base(binary), target)
		}
	}
	return ""
}

// Empty when the install directory is usable on target, the smoke run made only for the host target
func (s *Store) verifyEntry(ctx context.Context, r *recipe, entry string, target, host Target) string {
	if reason := r.fileEvidence(entry, target); reason != "" {
		return reason
	}
	binaries, _ := r.binaries(entry, target)
	if len(r.probe) == 0 || target != host || target.bionic() || len(binaries) == 0 {
		return ""
	}
	return s.runProbe(ctx, r, binaries[0])
}

// Runs the smoke command of a program and returns why it failed, empty when it exited zero
func (s *Store) runProbe(ctx context.Context, r *recipe, binary string) string {
	shown := binary + " " + strings.Join(r.probe, " ")
	bounded, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.Command(binary, r.probe...)
	if r.probeDir {
		cmd.Dir = filepath.Dir(binary)
	}
	if r.probeEnv != nil {
		cmd.Env = r.probeEnv(ctx, s)
	}
	res, err := proc.Run(bounded, cmd)
	switch {
	case err != nil && ctx.Err() == nil && errors.Is(bounded.Err(), context.DeadlineExceeded):
		return fmt.Sprintf("%s timed out after %gs", shown, probeTimeout.Seconds())
	case err != nil:
		return fmt.Sprintf("could not exec %s: %v", shown, err)
	case res.Exit.Success():
		return ""
	}
	tail := strings.TrimSpace(strings.ToValidUTF8(string(res.Stdout)+string(res.Stderr), "�"))
	if runes := []rune(tail); len(runes) > 300 {
		tail = string(runes[len(runes)-300:])
	}
	reason := fmt.Sprintf("%s exited %d", shown, res.Exit.Status())
	if tail != "" {
		reason += ": " + tail
	}
	return reason
}

// Requires the release checksum list to name the pinned archive digest, then checks its signature
func verifyProvenance(ctx context.Context, name, staged string, archive Artifact) error {
	dir := filepath.Join(staged, provenanceDir)
	text, err := fileutil.ReadText(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		return err
	}
	expected := ""
	for _, line := range strings.Split(text, "\n") {
		if row := strings.Fields(line); len(row) == 2 && strings.TrimLeft(row[1], "*") == archive.fileName() {
			expected = row[0]
			break
		}
	}
	if expected != archive.SHA256 {
		return &SignatureError{Name: name, Reason: "release checksums do not cover the pinned archive"}
	}
	signature, key := filepath.Join(dir, "checksums.txt.asc"), filepath.Join(dir, "public-key.asc")
	for _, p := range []string{signature, key} {
		if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
			return &SignatureError{Name: name, Reason: "pinned signature assets missing"}
		}
	}
	decided, err := verifyDetached(key, signature, filepath.Join(dir, "checksums.txt"))
	if err != nil {
		return &SignatureError{Name: name, Reason: "GPG signature verification failed"}
	}
	if decided {
		return nil
	}
	return verifyWithGPG(ctx, name, key, signature, filepath.Join(dir, "checksums.txt"))
}

// Checks an armored detached signature in process, decided false when the key or signature format is beyond this reader
func verifyDetached(keyPath, signaturePath, signedPath string) (bool, error) {
	keyFile, err := os.Open(keyPath)
	if err != nil {
		return false, nil
	}
	keyring, err := openpgp.ReadArmoredKeyRing(keyFile)
	_ = keyFile.Close()
	if err != nil || len(keyring) == 0 {
		return false, nil
	}
	signed, err := os.Open(signedPath)
	if err != nil {
		return false, nil
	}
	defer signed.Close()
	signature, err := os.Open(signaturePath)
	if err != nil {
		return false, nil
	}
	defer signature.Close()
	_, err = openpgp.CheckArmoredDetachedSignature(keyring, signed, signature)
	var bad pgperrors.SignatureError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &bad):
		return true, err
	}
	return false, nil
}

// Checks the signature with gpg in a throwaway keyring, a rejection fatal and a missing tool or key import logged
func verifyWithGPG(ctx context.Context, name, key, signature, signed string) error {
	gpg, err := exec.LookPath("gpg")
	if err != nil {
		slog.Warn(fmt.Sprintf("gpg unavailable; %s archive checksum remains enforced", name))
		return nil
	}
	homeDir, err := os.MkdirTemp("", "icebath-iron-signature-")
	if err != nil {
		return err
	}
	defer fileutil.RemoveTree(homeDir, true)
	run := func(args ...string) (bool, error) {
		bounded, cancel := context.WithTimeout(ctx, signatureToolTimeout)
		defer cancel()
		cmd := exec.Command(gpg, append([]string{"--homedir", homeDir, "--batch", "--no-tty"}, args...)...)
		res, err := proc.Run(bounded, cmd)
		if err != nil {
			return false, err
		}
		return res.Exit.Success(), nil
	}
	imported, err := run("--import", key)
	if err != nil {
		return err
	}
	if !imported {
		slog.Warn(fmt.Sprintf("Could not import %s signing key; archive checksum remains enforced", name))
		return nil
	}
	verified, err := run("--verify", signature, signed)
	if err != nil {
		return err
	}
	if !verified {
		return &SignatureError{Name: name, Reason: "GPG signature verification failed"}
	}
	return nil
}

// Restores the execute bits of a program whose archive dropped them, false when it stays unrunnable
func healExecBit(binary string) bool {
	if fileutil.Accessible(binary, fileutil.AccessExec) {
		return true
	}
	fi, err := os.Stat(binary)
	if err != nil {
		return false
	}
	if err := os.Chmod(binary, fi.Mode().Perm()|0o111); err != nil && !errors.Is(err, fs.ErrPermission) {
		return false
	}
	return fileutil.Accessible(binary, fileutil.AccessExec)
}
