package backend

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

// Error text as String(error) prints a Node file system error
func nodeErrorString(err error) string {
	return "Error: " + hardening.NodeErr(err).Error()
}

// Removes only the absolute same name links into the payload bin that the provisioner creates
func RemoveBundleCLILinks(payloadRoot, binDir string) error {
	if _, err := os.Stat(binDir); err != nil {
		return nil
	}
	payloadBin, err := filepath.Abs(filepath.Join(payloadRoot, "bin"))
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return hardening.NodeErr(err)
	}
	for _, entry := range entries {
		if entry.Type()&fs.ModeSymlink == 0 {
			continue
		}
		link := filepath.Join(binDir, entry.Name())
		destination, err := os.Readlink(link)
		if err != nil {
			return hardening.NodeErr(err)
		}
		if filepath.IsAbs(destination) && filepath.Dir(destination) == payloadBin && filepath.Base(destination) == entry.Name() {
			if err := os.Remove(link); err != nil {
				return hardening.NodeErr(err)
			}
		}
	}
	return nil
}

// Links each payload command into binDir, repointing only dangling links of an earlier bundle
func ProvisionCLILinks(commands PayloadCommands, binDir string, log func(string)) {
	if err := os.MkdirAll(binDir, 0o777); err != nil {
		log("[payload] CLI PATH provision skipped: " + nodeErrorString(err))
		return
	}
	linked := 0
	for _, command := range commands {
		target := filepath.Join(binDir, filepath.Base(command.Path))
		done, err := provisionCLILink(command.Path, target)
		if err != nil {
			log(fmt.Sprintf("[payload] CLI PATH provision skipped for %s: %s", target, err.Error()))
			continue
		}
		if done {
			linked++
		}
	}
	if linked > 0 {
		log(fmt.Sprintf("[payload] linked %d CLI trampoline(s) into %s", linked, binDir))
	}
}

// Creates or repoints one link, false when an existing entry is left alone
func provisionCLILink(source, target string) (bool, error) {
	name := filepath.Base(source)
	existing, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		if err := os.Symlink(source, target); err != nil {
			return false, errors.New(nodeErrorString(err))
		}
		return true, nil
	}
	if err != nil {
		return false, errors.New(nodeErrorString(err))
	}
	if existing.Mode()&fs.ModeSymlink == 0 {
		return false, nil
	}
	if _, err := os.Stat(target); err == nil {
		return false, nil
	} else if code := hardening.NodeCode(err); code != "ENOENT" && code != "ENOTDIR" {
		return false, errors.New(nodeErrorString(err))
	}
	destination, err := os.Readlink(target)
	if err != nil {
		return false, errors.New(nodeErrorString(err))
	}
	bin := filepath.Dir(destination)
	if !filepath.IsAbs(destination) || filepath.Base(destination) != name || filepath.Base(bin) != "bin" || filepath.Base(filepath.Dir(bin)) != "agent-payload" {
		return false, nil
	}
	staged := target + ".icebath-provision-" + uuid.NewString()
	if err := os.Symlink(source, staged); err != nil {
		return false, errors.New(nodeErrorString(err))
	}
	if err := os.Rename(staged, target); err != nil {
		renameErr := nodeErrorString(err)
		if cleanupErr := os.Remove(staged); cleanupErr != nil && !errors.Is(cleanupErr, fs.ErrNotExist) {
			return false, fmt.Errorf("AggregateError: %s; temporary link cleanup failed: %s", renameErr, nodeErrorString(cleanupErr))
		}
		return false, errors.New(renameErr)
	}
	return true, nil
}
