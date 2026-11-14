package toolchain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/proc"
)

// Longest the PortableGit self extractor may run
const selfExtractTimeout = 600 * time.Second

// Path the entry map gives the entry point of target below dir, whether or not it exists
func (st *staging) expectedBinary() string {
	pattern, ok := lookupTarget(st.recipe.entry, st.target)
	if !ok || pattern == "" {
		return ""
	}
	return filepath.Join(st.tree, filepath.FromSlash(pattern))
}

// Gives the staged program mode 0755 when its archive stored it without execute bits
func stageExecutable(_ context.Context, st *staging) error {
	binary := st.expectedBinary()
	if fi, err := os.Stat(binary); binary == "" || err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	return os.Chmod(binary, 0o755)
}

// Chromium executables of an install directory for target, in path order
func findChromium(dir string, target Target) []string {
	if target.windows() {
		return filesNamed(dir, "chrome.exe")
	}
	return filesNamed(dir, "chrome", "chromium", "Google Chrome for Testing", "Chromium")
}

// Writes the marker Playwright reads as a finished browser install
func stageChromium(_ context.Context, st *staging) error {
	return os.WriteFile(filepath.Join(st.tree, "INSTALLATION_COMPLETE"), nil, 0o644)
}

// Drops the SDK the driver archive carries beside the program
func stageCuaDriver(_ context.Context, st *staging) error {
	sdk := filepath.Join(st.tree, "sdk")
	if fi, err := os.Stat(sdk); err == nil && fi.IsDir() {
		_ = fileutil.RemoveTree(sdk, true)
	}
	return nil
}

// Runs a scratch copy of the PortableGit self extractor into dest without pipes its children could hold
func unpackPortableGit(ctx context.Context, st *staging, _ int, archive, dest string) (bool, error) {
	name := st.recipe.name
	if hostinfo.OSFamily != hostinfo.FamilyWindows {
		return false, &InstallError{
			Name:   name,
			Cause:  "the pinned artifact is a PortableGit self-extracting 7z, which only runs on Windows",
			Remedy: "stage win32 git on a Windows host; POSIX hosts use the system git",
		}
	}
	if err := removeAll(dest); err != nil {
		return false, err
	}
	if err := os.MkdirAll(dest, 0o777); err != nil {
		return false, err
	}
	work, err := os.MkdirTemp(filepath.Dir(dest), ".sfx-")
	if err != nil {
		return false, err
	}
	defer fileutil.RemoveTree(work, true)
	exe := filepath.Join(work, filepath.Base(archive))
	if err := fileutil.CopyFile(archive, exe); err != nil {
		return false, err
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer null.Close()
	bounded, cancel := context.WithTimeout(ctx, selfExtractTimeout)
	defer cancel()
	cmd := exec.Command(exe, "-o"+dest, "-y")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
	res, err := proc.Run(bounded, cmd)
	switch {
	case err != nil && ctx.Err() == nil && errors.Is(bounded.Err(), context.DeadlineExceeded):
		return false, &InstallError{Name: name, Cause: fmt.Sprintf("PortableGit self-extractor did not finish in %g s", selfExtractTimeout.Seconds())}
	case err != nil:
		return false, err
	case !res.Exit.Success():
		return false, &InstallError{Name: name, Cause: fmt.Sprintf(
			"PortableGit self-extractor exited %d (it reports nothing under -y; usual causes: disk full, path-length limit, antivirus lock)",
			res.Exit.Status())}
	}
	return false, nil
}

// Files a signed release under the provenance directory and unpacks its archive flat
func unpackSigned(ctx context.Context, st *staging, index int, archive, dest string) (bool, error) {
	base := filepath.Base(archive)
	if provenanceName(base) {
		dir := filepath.Join(st.tree, provenanceDir)
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return false, err
		}
		data, err := os.ReadFile(archive)
		if err != nil {
			return false, err
		}
		return false, os.WriteFile(filepath.Join(dir, base), data, 0o644)
	}
	if err := extract(ctx, st.recipe.name, archive, dest); err != nil {
		return false, err
	}
	return index > 0, flattenSingleDir(ctx, dest)
}

// Requires a regular program file, makes it executable and checks the release provenance covers the pinned archive
func stageSigned(ctx context.Context, st *staging) error {
	binary := st.expectedBinary()
	fi, err := os.Lstat(binary)
	if binary == "" || err != nil || !fi.Mode().IsRegular() {
		return &InstallError{Name: st.recipe.name, Cause: "archive binary is not a regular file"}
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		return err
	}
	return verifyProvenance(ctx, st.recipe.name, st.tree, st.artifacts[0])
}

// Hoists the server out of a nested build directory and keeps only the OpenMP library of its runtime package
func stageLlama(ctx context.Context, st *staging) error {
	server := filepath.Base(st.expectedBinary())
	if fi, err := os.Stat(filepath.Join(st.tree, server)); err != nil || !fi.Mode().IsRegular() {
		found := pathsNamed(st.tree, server)
		if len(found) == 0 {
			return &InstallError{Name: st.recipe.name, Cause: "archive contains no llama-server"}
		}
		if err := mergeTree(ctx, filepath.Dir(found[0]), st.tree); err != nil {
			return err
		}
	}
	libs, err := filepath.Glob(filepath.Join(st.tree, "usr", "lib", "*", "libgomp.so.1"))
	if err != nil || len(libs) == 0 {
		return err
	}
	sort.Strings(libs)
	data, err := os.ReadFile(libs[0])
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(st.tree, "libgomp.so.1"), data, 0o644); err != nil {
		return err
	}
	return fileutil.RemoveTree(filepath.Join(st.tree, "usr"), true)
}

// Paths below dir with the base name, whatever they are, in path order
func pathsNamed(dir, name string) []string {
	var found []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != dir && d.Name() == name {
			found = append(found, p)
		}
		return nil
	})
	sort.Strings(found)
	return found
}

// Stages the server, then moves the CUDA runtime libraries out of their own directory to sit beside it
func stageLlamaCUDA(ctx context.Context, st *staging) error {
	if err := stageLlama(ctx, st); err != nil {
		return err
	}
	extras, err := filepath.Glob(filepath.Join(st.tree, "cudart-*"))
	if err != nil {
		return err
	}
	sort.Strings(extras)
	for _, extra := range extras {
		if fi, err := os.Stat(extra); err != nil || !fi.IsDir() {
			continue
		}
		if err := mergeTree(ctx, extra, st.tree); err != nil {
			return err
		}
		if err := fileutil.RemoveTree(extra, true); err != nil {
			return err
		}
	}
	return nil
}

// Unpacks the search tool into the tree and the runtime archive of a second pin beside it, unmerged
func unpackRipgrepRuntime(ctx context.Context, st *staging, _ int, archive, dest string) (bool, error) {
	return false, extract(ctx, st.recipe.name, archive, dest)
}

// Places the ARM64 C runtime library beside the Windows ARM64 search tool that links it dynamically
func stageRipgrep(_ context.Context, st *staging) error {
	if st.target != WindowsARM64 {
		return nil
	}
	found := filesNamed(filepath.Join(st.scratch, "extra-1"), "vcruntime140.dll")
	if len(found) == 0 {
		return &InstallError{Name: st.recipe.name, Cause: "ARM64 rg.exe needs vcruntime140.dll, which the pinned runtime archive does not hold"}
	}
	return fileutil.CopyFile(found[0], filepath.Join(st.tree, "vcruntime140.dll"))
}
