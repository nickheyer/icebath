package plugins

import "context"

const (
	computerUseProviderBase    = "tools.computer_use.backend:ComputerUseProvider"
	computerUseProviderCapture = "register_computer_use_provider"
)

// Loads the computer use provider a wasm or program plugin directory registers, nil when it registers none
func LoadComputerUseProviderInHost(ctx context.Context, providerDir string) (*HostedObject, error) {
	return LoadCategoryInstance(ctx, providerDir, computerUseProviderBase, computerUseProviderCapture, nil)
}
