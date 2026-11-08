package browser

import "github.com/nickheyer/icebath/pkg/tools"

// Tool result note when the real profile cannot load and a fresh signed out browser serves instead
func realProfileFallbackNote(err string) string {
	return err + " Using a fresh browser without the user's logins for this session instead; " +
		"tell the user if the task needs their accounts."
}

// Launcher key of the current profile's real profile browser, one per profile home
func realProfileKey(env *tools.Env) string { return "icebath-real-profile@" + cfgOf(env).homeKey() }
