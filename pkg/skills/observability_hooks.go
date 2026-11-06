package skills

import (
	"io/fs"

	"github.com/nickheyer/icebath/pkg/observability"
)

// Installs the skill index walk and the embedded bundled and optional trees the install snapshot and catalog read
func init() {
	observability.SkillIndexFilesHook = IterSkillIndexFiles
	observability.BundledSkillTreesHook = func() (fs.FS, fs.FS) { return BundledFS(), OptionalFS() }
}
