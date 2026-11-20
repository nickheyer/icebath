package gatewayrun

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Platforms whose adapters create a handoff thread a branch can move into
var branchThreadPlatforms = map[gateway.Platform]bool{gateway.PlatformDiscord: true, gateway.PlatformTelegram: true,
	gateway.PlatformSlack: true, gateway.PlatformMatrix: true}

// Leading branch flag that keeps the branch in the current chat
const branchHereFlag = "--here"

// Whether the branch stays here and its optional title, only a leading here flag counting as a flag
func parseBranchArgs(raw string) (bool, string) {
	text := pystr.Strip(raw)
	head, rest, _ := strings.Cut(text, " ")
	if pystr.Lower(head) == branchHereFlag {
		return true, pystr.Strip(rest)
	}
	return false, text
}

// Chat able to host a sibling thread for the source, nil for Discord DMs and threads with no known parent
func branchThreadParent(source *gateway.SessionSource) *string {
	if !branchThreadPlatforms[source.Platform] {
		return nil
	}
	if source.Platform == gateway.PlatformDiscord {
		if source.ChatType == "dm" {
			return nil
		}
		if (source.ThreadID != nil && *source.ThreadID != "") || source.ChatType == "thread" {
			if source.ParentChatID != nil && *source.ParentChatID != "" {
				parent := *source.ParentChatID
				return &parent
			}
			return nil
		}
	}
	if source.ChatID == "" {
		return nil
	}
	chat := source.ChatID
	return &chat
}

// Source a follow up typed in the new thread arrives on, per message fields dropped and identity kept
func branchDestSource(source *gateway.SessionSource, parentID, threadID, title string) *gateway.SessionSource {
	common := func(s *gateway.SessionSource) {
		thread := threadID
		s.ThreadID, s.MessageID, s.ProspectiveThreadID = &thread, nil, nil
		s.AutoThreadCreated, s.AutoThreadInitialName = false, nil
	}
	if source.Platform == gateway.PlatformDiscord {
		return replacedSource(source, func(s *gateway.SessionSource) {
			s.ChatID = threadID
			if title != "" {
				name := title
				s.ChatName = &name
			}
			s.ChatType = "thread"
			parent := parentID
			s.ParentChatID = &parent
			common(s)
		})
	}
	chatType := source.ChatType
	if chatType == "thread" {
		chatType = "group"
	}
	return replacedSource(source, func(s *gateway.SessionSource) {
		s.ChatID, s.ChatType = parentID, chatType
		common(s)
	})
}

// Clickable thread pointer on Discord, the backticked id elsewhere
func formatThreadRef(platform gateway.Platform, threadID string) string {
	if platform == gateway.PlatformDiscord {
		return "<#" + threadID + ">"
	}
	return "`" + threadID + "`"
}
