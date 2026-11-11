package selfupdate

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestSpawnedRestartWatcherIsNotIdentifiedAsAGateway(t *testing.T) {
	cmdline := "/opt/icebath/icebath " + gateway.GatewayRestartWatcherEntry + ` "" {} 14980 python -m icebath_cli.main gateway run`
	if sub, ok := hermesHolderSubcommand(cmdline); !ok || sub != gateway.GatewayRestartWatcherEntry {
		t.Fatalf("holder subcommand %q %v", sub, ok)
	}
	if isRuntimeHost([]string{cmdline}) {
		t.Fatalf("%q", cmdline)
	}
}
