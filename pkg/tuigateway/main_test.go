package tuigateway

import (
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	tools.ProfileSecrets = provider.ProfileSecretSource()
	BindIdentity(testIsAuthenticatedIdentity, func() string { return testHostOwner })
	os.Exit(m.Run())
}

// Subject the host owner double reports
const testHostOwner = "host-owner-subject"

// Login check double, a named user and provider other than the server internal credential
func testIsAuthenticatedIdentity(identity any) bool {
	fields, ok := identity.(map[string]any)
	if !ok {
		return false
	}
	userID, _ := fields["user_id"].(string)
	provider, _ := fields["provider"].(string)
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(provider) == "" {
		return false
	}
	return !(userID == "server-internal" && provider == "server-internal")
}
