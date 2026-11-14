package dashboard

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestMain(m *testing.M) {
	home.DispatchScratchPruneEntry(context.Background())
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	tools.ProfileSecrets = provider.ProfileSecretSource()
	if h, probing := os.LookupEnv(sessionListenerProbeEnv); probing {
		os.Exit(runSessionListenerProbe(h))
	}
	os.Exit(m.Run())
}
