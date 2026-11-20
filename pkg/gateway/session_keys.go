package gateway

import (
	"context"
	"strings"
)

// Whether a non DM session is shared across participants under the key isolation rules
func IsSharedMultiUserSession(source *SessionSource, groupSessionsPerUser, threadSessionsPerUser bool) bool {
	if source.ChatType == "dm" {
		return false
	}
	if source.ThreadID != nil && *source.ThreadID != "" {
		return !threadSessionsPerUser
	}
	return !groupSessionsPerUser
}

// Key prefix agent main for the default profile, agent main tilde for a profile named main, else agent and the name
func SessionKeyNamespace(profile string) string {
	if profile == "" || profile == "default" {
		return "agent:main"
	}
	if profile == "main" {
		return "agent:main~"
	}
	return "agent:" + profile
}

// Sender id for key isolation with WhatsApp aliases folded to one canonical identifier
func canonicalParticipant(ctx context.Context, source *SessionSource) (string, error) {
	participant := ""
	if source.UserIDAlt != nil && *source.UserIDAlt != "" {
		participant = *source.UserIDAlt
	} else if source.UserID != nil {
		participant = *source.UserID
	}
	if participant != "" && source.Platform == PlatformWhatsApp {
		canonical, err := CanonicalWhatsAppIdentifier(ctx, participant)
		if err != nil {
			return "", err
		}
		if canonical != "" {
			participant = canonical
		}
	}
	return participant, nil
}

// Deterministic session key of a message source, the pinned detached route winning
func BuildSessionKey(ctx context.Context, source *SessionSource, groupSessionsPerUser, threadSessionsPerUser bool, profile string) (string, error) {
	if source.DetachedRoute != nil {
		return *source.DetachedRoute, nil
	}
	isDM := source.ChatType == "dm"
	chatID := source.ChatID
	if isDM && source.Platform == PlatformWhatsApp {
		canonical, err := CanonicalWhatsAppIdentifier(ctx, chatID)
		if err != nil {
			return "", err
		}
		chatID = canonical
	}
	ownThread := ""
	if source.ThreadID != nil {
		ownThread = *source.ThreadID
	}
	threadID := ownThread
	if threadID == "" && !isDM && source.ProspectiveThreadID != nil {
		threadID = *source.ProspectiveThreadID
	}
	chatTypeSlot := source.ChatType
	if threadID != "" && ownThread == "" {
		chatTypeSlot = "thread"
	}
	var isolateUser bool
	if isDM {
		isolateUser = chatID == ""
	} else {
		isolateUser = groupSessionsPerUser && !(threadID != "" && !threadSessionsPerUser)
	}
	participant := ""
	if isolateUser || !isDM {
		var err error
		if participant, err = canonicalParticipant(ctx, source); err != nil {
			return "", err
		}
	}
	parts := []string{SessionKeyNamespace(profile), string(source.Platform), chatTypeSlot}
	if source.Platform == PlatformSlack && source.ScopeID != nil && *source.ScopeID != "" {
		parts = append(parts, *source.ScopeID)
	}
	if chatID != "" {
		parts = append(parts, chatID)
	}
	var userPart, threadPart []string
	if isolateUser && participant != "" {
		userPart = []string{participant}
	}
	if threadID != "" {
		threadPart = []string{threadID}
	}
	if isDM {
		parts = append(append(parts, userPart...), threadPart...)
	} else {
		parts = append(append(parts, threadPart...), userPart...)
	}
	return strings.Join(parts, ":"), nil
}
