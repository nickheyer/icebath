package config

import "fmt"

// Bundled platform plugin manifest as the diagnostics read it
type PlatformManifest struct {
	Name         string
	ManifestName string
	RequiredEnv  []string
}

// Readers the check diagnostics consult, supplied by the toolset validation and plugin discovery owners
type CheckDiagnosticsSources struct {
	PlatformToolsetDiagnostics func(platformToolsets any, cfg map[string]any) []string
	DisabledPlugins            func() map[string]bool
	BundledPlatformManifests   func() ([]PlatformManifest, error)
}

// Saved toolset problems plus each disabled bundled platform whose required credentials are all configured
func ConfigCheckDiagnostics(cfg map[string]any, getEnv func(name string) string, src CheckDiagnosticsSources) ([]string, error) {
	diagnostics := src.PlatformToolsetDiagnostics(cfg["platform_toolsets"], cfg)
	disabled := src.DisabledPlugins()
	manifests, err := src.BundledPlatformManifests()
	if err != nil {
		return nil, err
	}
	for _, m := range manifests {
		key := "platforms/" + m.Name
		if !disabled[key] && !disabled[m.ManifestName] {
			continue
		}
		if len(m.RequiredEnv) == 0 {
			continue
		}
		configured := true
		for _, env := range m.RequiredEnv {
			if getEnv(env) == "" {
				configured = false
				break
			}
		}
		if configured {
			diagnostics = append(diagnostics, fmt.Sprintf("platform plugin '%s' is disabled while its required credentials are configured. "+
				"Run `icebath plugins enable %s` if you want it active.", key, key))
		}
	}
	return diagnostics, nil
}
