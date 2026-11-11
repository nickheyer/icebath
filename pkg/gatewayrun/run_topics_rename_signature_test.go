package gatewayrun

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Discord adapter double with the rename_thread signature of the Python adapter method
type oneSignatureRenameAdapter struct{}

func (oneSignatureRenameAdapter) RenameThread(context.Context, string, string, gateway.ThreadRenameOptions) (bool, error) {
	return true, nil
}

// One Discord adapter serves the runner's title rename lane and the plugin set_thread_title verb
func TestOneRenameThreadSignatureServesRunnerAndPlatformActions(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var adapter any = oneSignatureRenameAdapter{}
	if _, ok := adapter.(threadRenameAdapter); !ok {
		t.Fatal("adapter does not satisfy the runner rename lane")
	}
	type pluginDiscordThreadAdapter interface {
		RenameThread(ctx context.Context, threadID, name string, opts plugins.ThreadRenameOptions) (bool, error)
	}
	if _, ok := adapter.(pluginDiscordThreadAdapter); !ok {
		t.Fatal("adapter does not satisfy the plugin set_thread_title verb")
	}
}
