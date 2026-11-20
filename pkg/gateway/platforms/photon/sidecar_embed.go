package photon

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugintree"
)

// Sidecar tree compiled into the binary beside the photon plugin manifest
const embeddedSidecarRoot = "platforms/photon/sidecar"

// Sidecar dir under the home kept equal to the compiled sidecar files, node_modules left in place for npm
func embeddedSidecarMirror(ctx context.Context) string {
	mirror := filepath.Join(home.Dir(ctx), "photon", "sidecar")
	if err := refreshMirrorFromFS(plugintree.Compiled(), embeddedSidecarRoot, mirror); err != nil {
		pkgLog().Warn(fmt.Sprintf("[photon] could not write the bundled sidecar to %s: %v", mirror, err))
	}
	return mirror
}

// Writes every changed sidecar file of the tree into the mirror
func refreshMirrorFromFS(tree fs.FS, root, mirror string) error {
	if err := os.MkdirAll(mirror, 0o777); err != nil {
		return err
	}
	for _, name := range mirrorFiles {
		data, err := fs.ReadFile(tree, path.Join(root, name))
		if err != nil {
			return err
		}
		dst := filepath.Join(mirror, name)
		if current, err := os.ReadFile(dst); err == nil && bytes.Equal(current, data) {
			continue
		}
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			return err
		}
	}
	return nil
}
