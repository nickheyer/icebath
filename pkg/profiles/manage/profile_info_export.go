package manage

import "context"

// One profile's info built from its directory with the skill tree counted synchronously
func ProfileInfoOf(ctx context.Context, name, path string, isDefault bool) (ProfileInfo, error) {
	return profileInfo(ctx, name, path, isDefault, "", false)
}
