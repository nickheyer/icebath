package config

// Setting of how many images one OpenRouter Image API call returns
const imageCountKey = "count"

// Earlier keys of the image count, an unquoted n reading as the YAML 1.1 boolean false
var retiredImageCountKeys = map[string]bool{"n": true, PyKeyString(false): true}

// Renames image_gen.openrouter n to count, an explicit count winning over the old key
func migrateTo59(r *migrationRun) error {
	cfg := r.read()
	imageGen, _ := cfg["image_gen"].(map[string]any)
	section, ok := imageGen["openrouter"].(map[string]any)
	if !ok {
		return nil
	}
	renamed := false
	for _, key := range MappingKeys(section) {
		if !retiredImageCountKeys[key] {
			continue
		}
		if _, has := section[imageCountKey]; !has {
			section[imageCountKey] = section[key]
		}
		delete(section, key)
		renamed = true
	}
	if !renamed {
		return nil
	}
	return r.commit(cfg, "image_gen.openrouter.n → count",
		"  ✓ Renamed image_gen.openrouter.n → count — YAML reads an unquoted n as false, so the image count now has a plain key.")
}
