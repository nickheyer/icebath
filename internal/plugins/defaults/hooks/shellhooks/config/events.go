package config

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
)

// One event a hook or a webhook hears of: its name in the settings file, what a person calls it, what a hook of it does, and the heading it
// is listed under
type Point struct {
	Name, Title, About, Group string
}

// Headings the events are listed under, in the order they show
const (
	GroupTools    = "Tools"
	GroupReplies  = "Replies"
	GroupSessions = "Sessions"
	GroupAsking   = "Commands and questions"
	GroupHelpers  = "Subagents and skills"
	GroupRequests = "Model requests"
	GroupChat     = "Chat apps"
	GroupBoard    = "Task board"
)

// Events hooks and webhooks hear of under their headings, in the order a person is shown them, those hooks are set on most first
var Points = []Point{
	{hooks.PreToolCall, "Before a tool runs", "Runs before each tool call and can block it, change it or ask you", GroupTools},
	{hooks.PostToolCall, "After a tool runs", "Runs after each tool call, with what the tool did", GroupTools},
	{hooks.TransformToolResult, "Tool results", "Can change the result of a tool before the model reads it", GroupTools},
	{hooks.TransformTerminalOutput, "Command output", "Can change the output of a terminal command before the model reads it", GroupTools},
	{hooks.PreLLMCall, "Before a reply starts", "Can refuse your message or give the agent more to go on", GroupReplies},
	{hooks.TransformLLMOutput, "The agent's reply", "Can rewrite the reply the agent ends with", GroupReplies},
	{hooks.PostLLMCall, "Reply answered", "Runs once the agent has answered your message", GroupReplies},
	{hooks.SessionEnd, "Reply ended", "Runs as each reply ends, also one that failed or was stopped", GroupReplies},
	{hooks.PreVerify, "Code changes checked", "Can send the agent back to check the code it changed", GroupReplies},
	{hooks.InterimMessage, "While the agent works", "Runs on what the agent says between its steps", GroupReplies},
	{hooks.StreamStart, "Reply starts", "Runs when a reply begins to stream", GroupReplies},
	{hooks.StreamDelta, "Reply streams", "Runs on each piece of a reply as it streams", GroupReplies},
	{hooks.StreamEnd, "Reply stops streaming", "Runs when a reply stops streaming", GroupReplies},
	{hooks.AgentLoopStopped, "Agent stops", "Runs when the agent stops working", GroupReplies},
	{hooks.SessionStart, "Session starts", "Runs when a session starts", GroupSessions},
	{hooks.SessionFinalize, "Session closes", "Runs when a session closes for good", GroupSessions},
	{hooks.SessionReset, "New session in a window", "Runs when a window leaves its session for a new one", GroupSessions},
	{hooks.SessionDelete, "Sessions deleted", "Runs when sessions are deleted", GroupSessions},
	{hooks.PostCompaction, "Session summarized", "Runs after a long session is summarized to fit the model", GroupSessions},
	{hooks.PreCommand, "Before a slash command", "Runs before a slash command, such as /model", GroupAsking},
	{hooks.PreApprovalRequest, "Before an approval", "Runs before the agent asks you to approve something", GroupAsking},
	{hooks.PostApprovalResponse, "Approval answered", "Runs once an approval was given, refused or not answered in time", GroupAsking},
	{hooks.HumanInputRequest, "You are asked something", "Runs when the agent asks you for an approval, an answer or a password", GroupAsking},
	{hooks.HumanInputResolved, "You answered", "Runs on what came of asking you", GroupAsking},
	{hooks.SubagentStart, "Subagent starts", "Runs when a subagent starts a task", GroupHelpers},
	{hooks.SubagentStop, "Subagent finishes", "Runs when a subagent finishes its task", GroupHelpers},
	{hooks.SkillLifecycle, "Skill used or changed", "Runs when a skill is loaded, read, changed or installed", GroupHelpers},
	{hooks.PreAPIRequest, "Before a model request", "Runs before each request goes to the provider", GroupRequests},
	{hooks.PostAPIRequest, "After a model request", "Runs on each answer from the provider", GroupRequests},
	{hooks.APIRequestError, "Model request failed", "Runs on each request to the provider that failed", GroupRequests},
	{hooks.PreAuxiliaryCall, "Before a helper request", "Runs before each request of a helper model", GroupRequests},
	{hooks.PostAuxiliaryCall, "After a helper request", "Runs on each answer to a helper model, or its failure", GroupRequests},
	{hooks.PreTranscription, "Speech to text starts", "Can set the model, language and prompt of a transcription", GroupRequests},
	{hooks.PreGatewayDispatch, "Chat app message arrives", "Runs on a message from a chat app before it is handled", GroupChat},
	{hooks.PostGatewayAdmission, "Chat app message let in", "Runs on a message from a chat app once it is let in", GroupChat},
	{hooks.GatewayPlatformEvent, "Chat app events", "Runs on the events of your chat apps", GroupChat},
	{hooks.RoomMemberActivity, "Room activity", "Runs on what the members of a room do", GroupChat},
	{hooks.KanbanTaskClaimed, "Board task taken", "Runs when a task on the board is taken", GroupBoard},
	{hooks.KanbanTaskCompleted, "Board task done", "Runs when a task on the board is done", GroupBoard},
	{hooks.KanbanTaskBlocked, "Board task blocked", "Runs when a task on the board is blocked", GroupBoard},
	{hooks.KanbanTaskUpdated, "Board task changed", "Runs when a task on the board changes", GroupBoard},
	{hooks.KanbanWorkerSpawned, "Board worker starts", "Runs when a worker of the board starts", GroupBoard},
	{hooks.KanbanWorkerExited, "Board worker exits", "Runs when a worker of the board exits", GroupBoard},
	{hooks.KanbanWorkerStaleClaim, "Board worker stalls", "Runs when a worker holds a task too long", GroupBoard},
	{hooks.KanbanDispatchTick, "Board hands out tasks", "Runs each time the board hands out tasks", GroupBoard},
}

// The event of the name as a person reads it, the name itself as its title for one icebath does not know
func PointOf(name string) Point {
	for _, point := range Points {
		if point.Name == name {
			return point
		}
	}
	return Point{Name: name, Title: name}
}

// Place of the event among the events as they are listed, those not listed last
func Place(name string) int {
	for i, point := range Points {
		if point.Name == name {
			return i
		}
	}
	return len(Points)
}

// The events as the values a list of them is ticked from
func pointOptions() []config.Option {
	options := make([]config.Option, len(Points))
	for i, point := range Points {
		options[i] = config.Option{Value: point.Name, Title: point.Title, About: point.About}
	}
	return options
}
