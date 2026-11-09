package profiles

import "github.com/nickheyer/icebath/pkg/tools"

// Hands the session_search tool the profile name and home resolvers of this package
func init() {
	tools.ProfileStoreHooks.ActiveName = GetActiveProfileName
	tools.ProfileStoreHooks.Normalize = NormalizeProfileName
	tools.ProfileStoreHooks.Validate = ValidateProfileName
	tools.ProfileStoreHooks.Exists = ProfileExists
	tools.ProfileStoreHooks.Dir = GetProfileDir
}
