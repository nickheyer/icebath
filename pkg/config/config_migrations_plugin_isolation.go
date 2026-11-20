package config

// Drops the plugin isolation mode, a setting nothing reads since plugins run as WebAssembly modules or external programs
func migrateTo69(r *migrationRun) error {
	cfg := r.read()
	plugins, _ := cfg["plugins"].(map[string]any)
	if _, has := plugins["isolation"]; !has {
		return nil
	}
	delete(plugins, "isolation")
	return r.commit(cfg, "removed plugins.isolation",
		"  ✓ Removed plugins.isolation — plugins run as WebAssembly modules or external programs, so nothing in icebath reads this setting.")
}
