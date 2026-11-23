package tuiserver

// Commands that queue onto pending input, routed straight to command.dispatch ahead of the skill and plugin stages
var pendingInputCommands = map[string]bool{"retry": true, "queue": true, "q": true, "steer": true, "plan": true, "goal": true,
	"loop": true, "proactive": true, "moa": true, "undo": true, "learn": true, "init": true, "compress": true, "compact": true}

// Snapshot command words, their restore form refused here since it mutates live config and state
var snapshotCommands = map[string]bool{"snapshot": true, "snap": true}
