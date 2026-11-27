package tools

// Variables naming the session a command runs for, which every command a session runs is given anew: those the session binds hold its
// values, the rest are cleared so a value the server's own environment carries never stands in
var SessionNames = []string{
	"ICEBATH_SESSION_PLATFORM", "ICEBATH_SESSION_SOURCE", "ICEBATH_SESSION_CHAT_ID", "ICEBATH_SESSION_CHAT_TYPE", "ICEBATH_SESSION_CHAT_NAME",
	"ICEBATH_SESSION_THREAD_ID", "ICEBATH_SESSION_USER_ID", "ICEBATH_SESSION_USER_ID_ALT", "ICEBATH_SESSION_USER_NAME", "ICEBATH_SESSION_SCOPE_ID",
	"ICEBATH_SESSION_KEY", "ICEBATH_SESSION_ID", "ICEBATH_UI_SESSION_ID", "ICEBATH_SESSION_MESSAGE_ID", "ICEBATH_SESSION_PROFILE",
	"ICEBATH_CRON_SESSION", "ICEBATH_SESSION_PARENT_CHAT_ID", "ICEBATH_CRON_AUTO_DELIVER_PLATFORM", "ICEBATH_CRON_AUTO_DELIVER_CHAT_ID",
	"ICEBATH_CRON_AUTO_DELIVER_THREAD_ID", DelegatedMarker,
}

// Variable a command of a subagent's session carries, so what it starts knows it works for a delegated agent
const DelegatedMarker = "ICEBATH_DELEGATED_CHILD_CONTEXT"

// Surfaces a session is shown on that are no chat platform, whose commands are not told a platform
var local = map[string]bool{"": true, "tui": true, "cli": true, "oneshot": true, "local": true, "desktop": true, "subagent": true}

// Variables of the session a call works for, by every name of SessionNames: the session's id and key, its profile, the platform of a
// session of a chat app and the marker of a subagent's session hold values, the other names hold none and are cleared; nil outside a
// session
func SessionVars(call *Call) map[string]string {
	if call == nil || call.SessionID == "" {
		return nil
	}
	vars := make(map[string]string, len(SessionNames))
	for _, name := range SessionNames {
		vars[name] = ""
	}
	vars["ICEBATH_SESSION_ID"], vars["ICEBATH_SESSION_KEY"] = call.SessionID, call.SessionID
	if env := call.Env; env != nil {
		if env.Config != nil {
			vars["ICEBATH_SESSION_PROFILE"] = env.Config.Profile()
		}
		if !local[env.Platform] {
			vars["ICEBATH_SESSION_PLATFORM"] = env.Platform
		}
		if env.Delegated != nil {
			vars[DelegatedMarker] = "1"
		}
	}
	return vars
}
