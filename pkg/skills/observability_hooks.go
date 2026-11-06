package skills

import (
	"maps"

	"github.com/nickheyer/icebath/pkg/observability"
)

// Hands the hub source alias table to the shared metrics extension registry
func init() {
	observability.SkillSourceIDAliasesHook = func() map[string]string { return maps.Clone(sourceIDAliases) }
}
