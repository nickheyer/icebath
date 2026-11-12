package skills

import (
	"context"
	"io/fs"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/observability"
)

// Installs the skill index walk and the embedded bundled and optional trees the install snapshot and catalog read
func init() {
	observability.SkillIndexFilesHook = IterSkillIndexFiles
	observability.BundledSkillTreesHook = func() (fs.FS, fs.FS) { return BundledFS(), OptionalFS() }
	observability.ResolveSkillCommandKeyHook = func(ctx context.Context, name string) (bool, error) {
		_, found, err := NewScope(home.Dir(ctx), nil).ResolveSkillCommandKey(ctx, name, false)
		return found, err
	}
}
