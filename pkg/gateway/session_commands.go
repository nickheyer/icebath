package gateway

// Registry commands whose reviewed read handlers the owner runs for a local session
var SessionReadCommands = map[string]bool{"help": true, "commands": true, "status": true, "context": true, "version": true,
	"whoami": true, "usage": true, "insights": true, "profile": true, "diff": true, "memory": true, "kanban": true,
	"topup": true, "suggestions": true, "bundles": true, "skills": true}
