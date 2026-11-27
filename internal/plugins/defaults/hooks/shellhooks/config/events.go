package config

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
)

// One event a hook or a webhook hears of: its name in the settings file, what a person calls it and what a hook of it does
type Point struct {
	Name, Title, About string
}

// Events hooks and webhooks hear of, in the order a person is shown them, those hooks are set on most first
var Points = []Point{
	{hooks.PreToolCall, "Before a tool runs", "Runs before each tool call and can block it, change it or ask you"},
	{hooks.PostToolCall, "After a tool runs", "Runs after each tool call, with what the tool did"},
	{hooks.TransformToolResult, "Tool results", "Can change the result of a tool before the model reads it"},
	{hooks.TransformTerminalOutput, "Command output", "Can change the output of a terminal command before the model reads it"},
	{hooks.PreLLMCall, "Before a reply starts", "Can refuse your message or give the agent more to go on"},
	{hooks.TransformLLMOutput, "The agent's reply", "Can rewrite the reply the agent ends with"},
	{hooks.PostLLMCall, "Reply answered", "Runs once the agent has answered your message"},
	{hooks.SessionEnd, "Reply ended", "Runs as each reply ends, also one that failed or was stopped"},
	{hooks.PreVerify, "Code changes checked", "Can send the agent back to check the code it changed"},
	{hooks.InterimMessage, "While the agent works", "Runs on what the agent says between its steps"},
	{hooks.StreamStart, "Reply starts", "Runs when a reply begins to stream"},
	{hooks.StreamDelta, "Reply streams", "Runs on each piece of a reply as it streams"},
	{hooks.StreamEnd, "Reply stops streaming", "Runs when a reply stops streaming"},
	{hooks.SessionStart, "Session starts", "Runs when a session starts"},
	{hooks.SessionFinalize, "Session closes", "Runs when a session closes for good"},
	{hooks.SessionReset, "New session in a window", "Runs when a window leaves its session for a new one"},
	{hooks.SessionDelete, "Sessions deleted", "Runs when sessions are deleted"},
	{hooks.PostCompaction, "Session summarized", "Runs after a long session is summarized to fit the model"},
	{hooks.PreCommand, "Before a slash command", "Runs before a slash command, such as /model"},
	{hooks.PreApprovalRequest, "Before an approval", "Runs before the agent asks you to approve something"},
	{hooks.PostApprovalResponse, "Approval answered", "Runs once an approval was given, refused or not answered in time"},
	{hooks.HumanInputRequest, "You are asked something", "Runs when the agent asks you for an approval, an answer or a password"},
	{hooks.HumanInputResolved, "You answered", "Runs on what came of asking you"},
	{hooks.SubagentStart, "Subagent starts", "Runs when a subagent starts a task"},
	{hooks.SubagentStop, "Subagent finishes", "Runs when a subagent finishes its task"},
	{hooks.SkillLifecycle, "Skill used or changed", "Runs when a skill is loaded, read, changed or installed"},
	{hooks.PreAPIRequest, "Before a model request", "Runs before each request goes to the provider"},
	{hooks.PostAPIRequest, "After a model request", "Runs on each answer from the provider"},
	{hooks.APIRequestError, "Model request failed", "Runs on each request to the provider that failed"},
	{hooks.PreAuxiliaryCall, "Before a helper request", "Runs before each request of a helper model"},
	{hooks.PostAuxiliaryCall, "After a helper request", "Runs on each answer to a helper model, or its failure"},
	{hooks.PreTranscription, "Speech to text starts", "Can set the model, language and prompt of a transcription"},
	{hooks.PreGatewayDispatch, "Chat app message arrives", "Runs on a message from a chat app before it is handled"},
	{hooks.PostGatewayAdmission, "Chat app message let in", "Runs on a message from a chat app once it is let in"},
	{hooks.GatewayPlatformEvent, "Chat app events", "Runs on the events of your chat apps"},
	{hooks.AgentLoopStopped, "Agent stops", "Runs when the agent stops working"},
	{hooks.RoomMemberActivity, "Room activity", "Runs on what the members of a room do"},
	{hooks.KanbanTaskClaimed, "Board task taken", "Runs when a task on the board is taken"},
	{hooks.KanbanTaskCompleted, "Board task done", "Runs when a task on the board is done"},
	{hooks.KanbanTaskBlocked, "Board task blocked", "Runs when a task on the board is blocked"},
	{hooks.KanbanTaskUpdated, "Board task changed", "Runs when a task on the board changes"},
	{hooks.KanbanWorkerSpawned, "Board worker starts", "Runs when a worker of the board starts"},
	{hooks.KanbanWorkerExited, "Board worker exits", "Runs when a worker of the board exits"},
	{hooks.KanbanWorkerStaleClaim, "Board worker stalls", "Runs when a worker holds a task too long"},
	{hooks.KanbanDispatchTick, "Board hands out tasks", "Runs each time the board hands out tasks"},
}

// The events as the values a list of them is ticked from
func pointOptions() []config.Option {
	options := make([]config.Option, len(Points))
	for i, point := range Points {
		options[i] = config.Option{Value: point.Name, Title: point.Title, About: point.About}
	}
	return options
}
