package toolchain

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/internal/proc"
)

// Attribute Gatekeeper reads to refuse a downloaded program and the bound on one codesign run
const (
	quarantineAttr  = "com.apple.quarantine"
	codesignTimeout = 60 * time.Second
)

// Runs codesign with args and returns whether it accepted, with its output for a diagnosis
func runCodesign(ctx context.Context, codesign string, args ...string) (bool, string) {
	bounded, cancel := context.WithTimeout(ctx, codesignTimeout)
	defer cancel()
	res, err := proc.Run(bounded, exec.Command(codesign, args...))
	if err != nil {
		return false, err.Error()
	}
	return res.Exit.Success(), strings.TrimSpace(string(res.Stderr) + string(res.Stdout))
}

// Clears the quarantine attribute from a staged tree and ad-hoc signs a bare entry point with no valid signature
func finishPlatform(ctx context.Context, tree string, binaries []string) error {
	_ = filepath.WalkDir(tree, func(p string, _ fs.DirEntry, err error) error {
		if err == nil {
			_ = unix.Lremovexattr(p, quarantineAttr)
		}
		return nil
	})
	if len(binaries) == 0 {
		return ctx.Err()
	}
	codesign, err := exec.LookPath("codesign")
	if err != nil {
		slog.Info("macOS codesign is unavailable; using the downloaded signatures")
		return ctx.Err()
	}
	for _, binary := range binaries {
		if strings.Contains(filepath.ToSlash(binary), ".app/Contents/") {
			continue
		}
		if ok, _ := runCodesign(ctx, codesign, "--verify", "--strict", binary); ok {
			continue
		}
		if ok, out := runCodesign(ctx, codesign, "--force", "--sign", "-", "--timestamp=none", binary); !ok {
			slog.Warn(fmt.Sprintf("could not sign %s: %s", binary, out))
		}
	}
	return ctx.Err()
}
