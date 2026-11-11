package config

import (
	"io/fs"
	"os"
	"path"

	"github.com/nickheyer/icebath/pkg/plugintree"
)

// Platform manifests of the bundled tree compiled into the binary, one per directory holding a regular plugin.yaml or plugin.yml
func compiledPlatformManifestPaths() []platformManifestPath {
	tree := plugintree.Compiled()
	entries, err := fs.ReadDir(tree, "platforms")
	if err != nil {
		panic("config: platforms tree missing from the compiled bundled plugins")
	}
	var out []platformManifestPath
	for _, entry := range entries {
		if !isPluginDirName(entry.Name()) || !entry.IsDir() {
			continue
		}
		for _, name := range []string{"plugin.yaml", "plugin.yml"} {
			manifest := path.Join("platforms", entry.Name(), name)
			fi, err := fs.Stat(tree, manifest)
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			out = append(out, platformManifestPath{dirName: entry.Name(), path: manifest, info: fi, fsys: tree})
			break
		}
	}
	return out
}

// Manifest bytes read from the tree the manifest was found in, the compiled tree or the disk
func (p platformManifestPath) read() ([]byte, error) {
	if p.fsys != nil {
		return fs.ReadFile(p.fsys, p.path)
	}
	return os.ReadFile(p.path)
}
