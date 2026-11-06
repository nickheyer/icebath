package config

// Directory names of every readable platform plugin manifest across the bundled and user sources
func PlatformPluginManifestDirNames(h string) ([]string, error) {
	manifests, err := platformPluginManifests(h, PlatformSourceAll, false, nil)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(manifests))
	for _, m := range manifests {
		out = append(out, m.DirName)
	}
	return out, nil
}

// Directory names of the bundled platform plugins that have a manifest path
func BundledPlatformDirNames(h string) []string {
	var out []string
	for _, p := range platformManifestPaths(h, PlatformSourceBundled) {
		if p.path != "" {
			out = append(out, p.dirName)
		}
	}
	return out
}
