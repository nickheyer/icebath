package plugins

import (
	"io/fs"
	"maps"
	"path"
	"strings"

	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/plugintree"
)

// Tier carrying the Go package of the bundled plugins under each top tree directory, the core for one not named
var bundledTreeTiers = map[string]string{
	"platforms": linktier.Plugins, "image_gen": linktier.Plugins, "video_gen": linktier.Plugins, "web": linktier.Plugins,
	"disk-cleanup": linktier.Plugins, "security-guidance": linktier.Plugins, "teams_pipeline": linktier.Plugins,
	"config_backends": linktier.Plugins, "browser": linktier.Extras, "computer_use": linktier.Extras, "google_meet": linktier.Extras,
}

// Tier carrying the compiled Go code of each catalog plugin a link file registers as a port
var catalogPortTiers = map[string]string{
	"spotify": linktier.Plugins, "camofox": linktier.Plugins, "mcp-serve": linktier.Plugins, "langfuse": linktier.Plugins,
	"homeassistant": linktier.Plugins, "agent-importers": linktier.Extras,
}

// Line naming the build that carries the compiled code of a catalog plugin this binary leaves out, false for any other name
func catalogTierRefusal(name string) (string, bool) {
	tier := catalogPortTiers[name]
	if tier == "" || linktier.Linked(tier) {
		return "", false
	}
	return linktier.Missing("the "+name+" plugin", tier), true
}

// Top tree directories whose tier this binary leaves out added to a scan's skip set
func skipAbsentTiers(skip map[string]bool) map[string]bool {
	out := maps.Clone(skip)
	if out == nil {
		out = map[string]bool{}
	}
	for dir, tier := range bundledTreeTiers {
		if !linktier.Linked(tier) {
			out[dir] = true
		}
	}
	return out
}

// Line naming the build that carries a compiled bundled plugin this binary leaves out, false for any other name
func bundledTierRefusal(name string) (string, bool) {
	if bundledOverrideDir() != "" {
		return "", false
	}
	tree := plugintree.Compiled()
	var line string
	var walk func(base, prefix string, depth int) bool
	walk = func(base, prefix string, depth int) bool {
		children, _ := fs.ReadDir(tree, base)
		for _, child := range children {
			d := path.Join(base, child.Name())
			if !child.IsDir() || depth == 0 && listingBundledSkip[child.Name()] {
				continue
			}
			manifestName, _, _, key, ok, err := readCompiledManifestInfo(tree, d, prefix)
			if err != nil {
				continue
			}
			if !ok {
				if depth == 0 && walk(d, child.Name(), 1) {
					return true
				}
				continue
			}
			if name != key && name != manifestName && name != path.Base(key) {
				continue
			}
			top, _, _ := strings.Cut(d, "/")
			if tier := bundledTreeTiers[top]; tier != "" && !linktier.Linked(tier) {
				line = linktier.Missing("the "+key+" plugin", tier)
				return true
			}
			return false
		}
		return false
	}
	return line, walk(".", "", 0)
}
