package plugins

import (
	"github.com/nickheyer/icebath/pkg/plugins/verify"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Hands the verify runner the process_identity ownership calls it cannot import itself
func init() {
	verify.SpawnOwned = tools.SpawnOwned
	verify.ReleaseProcessGroupOnceEmpty = tools.ReleaseProcessGroupOnceEmpty
}
