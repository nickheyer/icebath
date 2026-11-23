package settings

import "time"

// Send the model's earlier reasoning back to it on each request
var AgentReasoningEcho = Register(&Bool{Meta: Meta{Key: "agent.reasoning_echo", Label: "Echo reasoning back", Short: "Send the model's earlier reasoning back to it on each request.", Help: "Only needed for providers that require it.", Group: GroupWorking, Order: 1000, Advanced: true, Apply: NewSession}, Default: false})

// Settings for the arena view in the terminal UI
var DisplayArena = Register(&Map{Meta: Meta{Key: "display.arena", Label: "Arena layout", Short: "Settings for the arena view in the terminal UI.", Group: GroupWorking, Order: 1010, Advanced: true}})

// How much to report when a background command produces output
var DisplayBackgroundProcessNotifications = Register(&Enum{Meta: Meta{Key: "display.background_process_notifications", Label: "Background job notices", Short: "How much to report when a background command produces output.", Group: GroupWorking, Order: 1020, Advanced: true}, Default: "concise", Values: []Choice{
	{Value: "concise", Label: "Concise", Meaning: "Short summary when it finishes."},
	{Value: "all", Label: "All", Meaning: "Every update with its output."},
	{Value: "result", Label: "Result", Meaning: "Only the final result."},
	{Value: "error", Label: "Errors", Meaning: "Only when it fails."},
	{Value: "off", Label: "Off", Meaning: "No notices."},
}})

// Confirm in chat when a message sent mid-turn steers the agent
var DisplayBusySteerAckEnabled = Register(&Bool{Meta: Meta{Key: "display.busy_steer_ack_enabled", Platforms: true, Label: "Confirm steering", Short: "Confirm in chat when a message sent mid-turn steers the agent.", Group: GroupWorking, Order: 1030, Advanced: true}, Default: true})

// What a plain chat message does while the agent is working
var DisplayBusyTextMode = Register(&Enum{Meta: Meta{Key: "display.busy_text_mode", Label: "Busy chat text", Short: "What a plain chat message does while the agent is working.", Help: "Overrides Enter while busy for chat apps only.", Group: GroupWorking, Order: 1040, Advanced: true}, Null: "follows Enter while busy", Values: []Choice{
	{Value: "interrupt", Label: "Interrupt", Meaning: "Stop the turn and start the new message."},
	{Value: "queue", Label: "Queue", Meaning: "Wait and send after the turn ends."},
}})

// Seconds before short system notices in chat apps delete themselves
var DisplayEphemeralSystemTtl = Register(&Int{Meta: Meta{Key: "display.ephemeral_system_ttl", Label: "Notice lifetime", Short: "Seconds before short system notices in chat apps delete themselves.", Help: "Zero keeps them.", Group: GroupWorking, Order: 1050, Advanced: true}, Default: 0, Unit: "seconds"})

// List files the agent failed to edit at the end of a turn
var DisplayFileMutationVerifier = Register(&Bool{Meta: Meta{Key: "display.file_mutation_verifier", Label: "Report failed edits", Short: "List files the agent failed to edit at the end of a turn.", Group: GroupWorking, Order: 1060, Advanced: true}, Default: true})

// How markdown in the final answer is shown in the classic CLI
var DisplayFinalResponseMarkdown = Register(&Enum{Meta: Meta{Key: "display.final_response_markdown", Label: "Final answer markup", Short: "How markdown in the final answer is shown in the classic CLI.", Group: GroupWorking, Order: 1070, Advanced: true}, Default: "strip", Values: []Choice{
	{Value: "strip", Label: "Strip", Meaning: "Remove markup, keep plain text."},
	{Value: "render", Label: "Render", Meaning: "Draw headings, bold and lists."},
	{Value: "raw", Label: "Raw", Meaning: "Show the markup as written."},
}})

// Describe tool calls in plain words in chat apps
var DisplayFriendlyToolLabels = Register(&Bool{Meta: Meta{Key: "display.friendly_tool_labels", Platforms: true, Label: "Friendly tool names", Short: "Describe tool calls in plain words in chat apps.", Group: GroupWorking, Order: 1090, Advanced: true, Apply: NextTurn}, Default: true})

// Show file changes as a diff right in the transcript
var DisplayInlineDiffs = Register(&Bool{Meta: Meta{Key: "display.inline_diffs", Label: "Inline diffs", Short: "Show file changes as a diff right in the transcript.", Group: GroupWorking, Order: 1100, Advanced: true}, Default: true})

// Show the agent's short updates while it works
var DisplayInterimAssistantMessages = Register(&Bool{Meta: Meta{Key: "display.interim_assistant_messages", Platforms: true, Label: "Mid-turn messages", Short: "Show the agent's short updates while it works.", Help: "Needs Show commentary to be on as well in the terminal UI.", Group: GroupWorking, Order: 1110, Advanced: true}, Default: true})

// How shown reasoning is formatted in chat apps
var DisplayReasoningStyle = Register(&Enum{Meta: Meta{Key: "display.reasoning_style", Platforms: true, Label: "Reasoning style", Short: "How shown reasoning is formatted in chat apps.", Group: GroupWorking, Order: 1120, Advanced: true}, Default: "code", Values: []Choice{
	{Value: "code", Label: "Code block", Meaning: "Monospaced block."},
	{Value: "blockquote", Label: "Quote", Meaning: "Indented quote."},
	{Value: "subtext", Label: "Subtext", Meaning: "Small muted text."},
}})

// Show the agent's running commentary between tool calls
var DisplayShowCommentary = Register(&Bool{Meta: Meta{Key: "display.show_commentary", Label: "Show commentary", Short: "Show the agent's running commentary between tool calls.", Group: GroupWorking, Order: 1130, Advanced: true}, Default: true})

// Show a live count of tokens flowing next to the spinner
var DisplaySpinnerTokenFlow = Register(&Bool{Meta: Meta{Key: "display.spinner_token_flow", Label: "Spinner token flow", Short: "Show a live count of tokens flowing next to the spinner.", Group: GroupWorking, Order: 1140, Advanced: true}, Default: true})

// Show replies as they are written instead of all at once
var DisplayStreaming = Register(&Bool{Meta: Meta{Key: "display.streaming", Platforms: true, PlatformOnly: true, Label: "Stream replies", Short: "Show replies as they are written instead of all at once.", Group: GroupWorking, Order: 1150, Advanced: true}, Default: true})

// Do not send warning notices to chat apps
var DisplaySuppressWarningNotifications = Register(&Bool{Meta: Meta{Key: "display.suppress_warning_notifications", Platforms: true, Label: "Hide warning notices", Short: "Do not send warning notices to chat apps.", Group: GroupWorking, Order: 1160, Advanced: true}, Default: false})

// Characters of each tool call shown in chat progress lines
var DisplayToolPreviewLength = Register(&Int{Meta: Meta{Key: "display.tool_preview_length", Platforms: true, Label: "Tool preview length", Short: "Characters of each tool call shown in chat progress lines.", Help: "Zero shows the built-in default length.", Group: GroupWorking, Order: 1170, Advanced: true, Apply: NextTurn}, Default: 0, Unit: "characters"})

// Allow the command that changes tool progress detail in chat apps
var DisplayToolProgressCommand = Register(&Bool{Meta: Meta{Key: "display.tool_progress_command", Label: "Progress command", Short: "Allow the command that changes tool progress detail in chat apps.", Group: GroupWorking, Order: 1180, Advanced: true}, Default: false})

// Whether chat tool progress builds up in one message or many
var DisplayToolProgressGrouping = Register(&Enum{Meta: Meta{Key: "display.tool_progress_grouping", Platforms: true, Label: "Tool progress grouping", Short: "Whether chat tool progress builds up in one message or many.", Group: GroupWorking, Order: 1190, Advanced: true, Apply: NextTurn}, Default: "accumulate", Values: []Choice{
	{Value: "accumulate", Label: "Together", Meaning: "Add each call to one message."},
	{Value: "separate", Label: "Separate", Meaning: "One message per call."},
}})

// Say why a turn ended when it stops without a normal answer
var DisplayTurnCompletionExplainer = Register(&Bool{Meta: Meta{Key: "display.turn_completion_explainer", Label: "Explain odd endings", Short: "Say why a turn ended when it stops without a normal answer.", Group: GroupWorking, Order: 1200, Advanced: true}, Default: true})

// Show a one-line summary of time and activity after each turn
var DisplayTurnSummary = Register(&Bool{Meta: Meta{Key: "display.turn_summary", Label: "Turn summary", Short: "Show a one-line summary of time and activity after each turn.", Group: GroupWorking, Order: 1210, Advanced: true}, Default: true})

// Notices in a chat while a long tool call keeps running
var DisplayLongRunningNotifications = Register(&Enum{Meta: Meta{Key: "display.long_running_notifications", Platforms: true, Label: "Long call notices", Short: "Post a notice in chats while a long tool call keeps running.", Group: GroupWorking, Order: 1220, Advanced: true}, Default: "on", On: "on", Off: "off", Values: []Choice{{Value: "on", Label: "On", Meaning: "Name the tool that is running."}, {Value: "generic", Label: "Generic", Meaning: "Say only that work goes on."}, {Value: "off", Label: "Off", Meaning: "No notices."}}})

// Detail line in the reply sent when a chat message arrives mid-turn
var DisplayBusyAckDetail = Register(&Bool{Meta: Meta{Key: "display.busy_ack_detail", Platforms: true, Label: "Busy reply detail", Short: "Say what the agent is doing when a chat message arrives mid-turn.", Group: GroupWorking, Order: 1230, Advanced: true}, Default: true})

// Reply sent when a chat message arrives while the agent works
var DisplayBusyAckEnabled = Register(&Bool{Meta: Meta{Key: "display.busy_ack_enabled", Label: "Busy reply", Short: "Reply in a chat when a message arrives while the agent works.", Group: GroupWorking, Order: 1240, Advanced: true}, Default: true})

// Progress messages removed from a chat once the reply is in
var DisplayCleanupProgress = Register(&Bool{Meta: Meta{Key: "display.cleanup_progress", Platforms: true, Label: "Clean up progress", Short: "Delete tool progress messages from a chat once the reply is in.", Group: GroupWorking, Order: 1250, Advanced: true}, Default: false})

// Status line a chat app shows while the agent works
var DisplayLiveStatus = Register(&Enum{Meta: Meta{Key: "display.live_status", Platforms: true, Label: "Live status", Short: "What the chat app's status line says while the agent works.", Group: GroupWorking, Order: 1260, Advanced: true}, Default: "full", On: "full", Off: "off", Values: []Choice{{Value: "full", Label: "Full", Meaning: "The tool and what it works on."}, {Value: "verb", Label: "Verb", Meaning: "Only what kind of work it is."}, {Value: "off", Label: "Off", Meaning: "No status line."}}})

// The model's thinking posted as progress in chats
var DisplayThinkingProgress = Register(&Bool{Meta: Meta{Key: "display.thinking_progress", Platforms: true, Label: "Thinking in chats", Short: "Post the model's thinking as progress messages in chats.", Group: GroupWorking, Order: 1270, Advanced: true}, Default: false})

// Quiet gap that ends a burst of chat messages sent while the agent works
var DisplayBusyTextDebounceSeconds = Register(&Duration{Meta: Meta{Key: "display.busy_text_debounce_seconds", Label: "Busy message gap", Short: "Quiet time that closes a burst of chat messages sent mid-turn.", Help: "Messages in one burst are queued as one.", Group: GroupWorking, Order: 1280, Advanced: true, Apply: Restart}, Default: 350 * time.Millisecond, Min: at(time.Duration(0))})

// Longest a burst of chat messages sent while the agent works is held
var DisplayBusyTextHardCapSeconds = Register(&Duration{Meta: Meta{Key: "display.busy_text_hard_cap_seconds", Label: "Busy message cap", Short: "Longest a burst of chat messages sent mid-turn is held.", Group: GroupWorking, Order: 1290, Advanced: true, Apply: Restart}, Default: time.Second, Min: at(time.Duration(0))})
