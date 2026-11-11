package tuiserver

import "github.com/nickheyer/icebath/pkg/tuigateway"

// Whether a session belongs to a profile home, an empty home naming the launch profile, any matching when anyProfile
func liveProfileMatches(session *tuigateway.Session, profileHome string, anyProfile bool) bool {
	return anyProfile || strOr(session.Get("profile_home")) == profileHome
}

// Live unfinalized session whose lookup key is the stored id, restricted to one profile unless anyProfile
func findLiveSessionByKey(sessionKey, profileHome string, anyProfile bool) (string, *tuigateway.Session, bool) {
	for _, live := range tuigateway.LiveSessions() {
		session := live.Session
		if !truthy(session.Get("_finalized")) && sessionLookupKey(session, live.SID) == sessionKey &&
			liveProfileMatches(session, profileHome, anyProfile) {
			return live.SID, session, true
		}
	}
	return "", nil, false
}
