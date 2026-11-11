package skills

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Every configured hub source adapter in priority order, a taps file failure returned as the error
func (h *Hub) SourceRouter() (sources []SkillSource, err error) {
	defer hubRecover(&err)
	return h.createSourceRouter(nil), nil
}

// Meta and bundle of an identifier from one adapter, else the first metadata only hit
func ResolveSourceMetaAndBundle(ctx context.Context, identifier string, sources []SkillSource) (*SkillMeta, *SkillBundle, SkillSource) {
	return (&HubCLI{}).resolveSourceMetaAndBundle(ctx, identifier, sources)
}

// Directory name of each loadable skill keyed by its load name, disabled skills included when skipDisabled
func (s *Scope) CatalogDirNames(skipDisabled bool) map[string]string {
	out := map[string]string{}
	for _, e := range s.skillCatalog(skipDisabled, false) {
		if e.LoadName != "" {
			out[e.LoadName] = filepath.Base(filepath.Dir(e.Path))
		}
	}
	return out
}

// Creates a skill through the manage write path with no approval gate, the result object of the create action
func (s *Scope) CreateSkill(ctx context.Context, name, content string, category any) (*pyjson.Object, error) {
	return s.createSkill(ctx, name, content, category)
}

// Full SKILL.md rewrite of an existing skill, the result object of the edit action
func (s *Scope) EditSkillContent(ctx context.Context, name, content string) (*pyjson.Object, error) {
	return s.editSkill(ctx, name, content)
}
