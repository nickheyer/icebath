package pm

import (
	"context"
	"io"
	"os"
)

type syncOutputKey struct{}

// Writer a dependency sync prints operator messages to, the process stdout unless redirected
func SyncOutput(ctx context.Context) io.Writer {
	if w, ok := ctx.Value(syncOutputKey{}).(io.Writer); ok {
		return w
	}
	return os.Stdout
}

// SyncVenvOptions select the extras, plugin source, repair and eviction policy and the output of a venv sync
type SyncVenvOptions struct {
	Extras                   []string
	Explicit                 bool
	Repair                   bool
	Automatic                []string
	Carried                  []string
	ProjectRoot              string
	EvictIncompatiblePlugins bool
	Plugins                  PluginInput
	Stdout                   io.Writer
}
