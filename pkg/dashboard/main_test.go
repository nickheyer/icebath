package dashboard

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	tools.ProfileSecrets = provider.ProfileSecretSource()
	os.Exit(m.Run())
}
