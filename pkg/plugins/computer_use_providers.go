package plugins

import "context"

const computerUseProviderBase = "tools.computer_use.backend:ComputerUseProvider"

// Loads the computer use provider a guest plugin directory registers, nil when it registers none
func LoadGuestComputerUseProvider(ctx context.Context, providerDir string) (*HostedObject, error) {
	return LoadGuestProvider(ctx, providerDir, "computer_use", computerUseProviderBase, nil, false, nil)
}
