package skills

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// Lock entry a hub result names for callers outside the package such as the mcp catalog
func InstalledEntry(installed map[string]*pyjson.Object, identifier, source string) *pyjson.Object {
	return installedEntry(installed, identifier, source)
}

// Source list the CLI searches, the Sources seam when set else the hub's router
func (x *HubCLI) ResolvedSources() []SkillSource {
	return x.sources()
}
