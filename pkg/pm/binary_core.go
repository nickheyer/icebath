//go:build !icebath_core && !icebath_noextras

package pm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
)

// Project name of the binary core, the name plugin requirements on the core resolve to
const binaryCoreName = "icebath-agent"

// Locked packages of the set this host's target installs
func (s *RuntimeSet) served() ([]RuntimeRequirement, error) {
	reqs, err := s.Requirements()
	if err != nil {
		return nil, err
	}
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	var out []RuntimeRequirement
	for _, r := range reqs {
		if r.ServesTarget(target) {
			out = append(out, r)
		}
	}
	return out, nil
}

// Pyproject and uv.lock of the binary core, a virtual project without packages that plugin requirements resolve to
func binaryCoreProject() ([]byte, []byte) {
	name := tomliWString(CanonicalizeName(binaryCoreName))
	project := "[project]\nname = " + name + "\nversion = \"0.0.0\"\nrequires-python = \">=3.11\"\ndependencies = [\n]\n\n[tool.uv]\npackage = false\n"
	lock := "version = 1\nrevision = 3\nrequires-python = \">=3.11\"\n\n[[package]]\nname = " + name +
		"\nversion = \"0.0.0\"\nsource = { virtual = \".\" }\ndependencies = [\n]\n\n[package.metadata]\nrequires-dist = []\n"
	return []byte(project), []byte(lock)
}

// Directory of the install state holding the binary core project
func binaryCoreDir(ctx context.Context, root string) string {
	return filepath.Join(InstallStateDir(ctx, root), "core")
}

// Writes the binary core project into the install state where its bytes differ, returning its directory
func materializeBinaryCore(ctx context.Context, root string) (string, error) {
	project, lock := binaryCoreProject()
	dir := binaryCoreDir(ctx, root)
	for name, data := range map[string][]byte{"pyproject.toml": project, "uv.lock": lock} {
		file := filepath.Join(dir, name)
		if current, err := os.ReadFile(file); err == nil && bytes.Equal(current, data) {
			continue
		}
		if err := DurableWriteBytes(file, data); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// Core project a generation resolves against, the checkout itself or the binary core of a binary install
func coreSource(ctx context.Context, root string) (string, error) {
	if !binaryCoreTree(root) {
		return root, nil
	}
	return materializeBinaryCore(ctx, root)
}

// Context whose store holds the toolchain a generation builds on, a binary install's state store fetched with progress on out
func coreToolchainContext(ctx context.Context, root string, out io.Writer) (context.Context, error) {
	if !binaryCoreTree(root) {
		return ctx, nil
	}
	toolchainCtx, _, found, err := runtimeToolchainContext(ctx, filepath.Join(InstallStateDir(ctx, root), "extras"))
	if err != nil || found {
		return toolchainCtx, err
	}
	if err := ensureRuntimeToolchain(toolchainCtx, out); err != nil {
		return nil, err
	}
	return toolchainCtx, nil
}

// Sha256 of the core lock a stamp hashes, the binary core lock for a binary install
func coreLockDigest(root string) ([]byte, error) {
	if !binaryCoreTree(root) {
		return uvLockDigest(filepath.Join(root, "uv.lock"))
	}
	_, lock := binaryCoreProject()
	sum := sha256.Sum256(lock)
	return sum[:], nil
}
