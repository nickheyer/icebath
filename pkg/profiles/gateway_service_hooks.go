package profiles

import "context"

// Gateway service naming and Windows task facts pkg/gatewaycli installs in init, nil in a binary that does not link it
var GatewayServiceHooks struct {
	ServiceName                   func(ctx context.Context) string
	UserSystemdUnitDir            func(ctx context.Context) string
	SystemUnitDir                 func() string
	LaunchdPlistPath              func(ctx context.Context) string
	PinnedHome                    func(definition string) (string, bool)
	WindowsTaskName               func(ctx context.Context) string
	WindowsTaskRegistered         func(ctx context.Context) bool
	ExecSchtasks                  func(args []string) (int, string, string)
	WindowsStartupEntryPath       func(ctx context.Context) string
	WindowsLegacyStartupEntryPath func(ctx context.Context) string
	ServiceManagerDetect          func() (string, error)
	ServiceManagerGet             func() (ProfileServiceManager, error)
}

// Service manager able to register profile gateways at runtime inside a container
type ProfileServiceManager interface {
	SupportsRuntimeRegistration() bool
	RegisterProfileGateway(name string, startNow bool) error
	UnregisterProfileGateway(name string) error
}
