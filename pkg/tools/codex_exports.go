package tools

// DelegatedChildEnvMarker carries the delegate child fence across a spawn, the exported face for the codex app server
const DelegatedChildEnvMarker = delegatedChildEnvMarker

// Worker identity keys scrubbed from every descendant, the exported face for the codex app server spawn
func KanbanEnvKeys() []string { return append([]string(nil), kanbanEnvKeys...) }

// False when a kanban identity is present but this execution does not own it, exported for the codex spawn
func IsDispatcherOwnedWorkerContext(env *Env) bool { return isDispatcherOwnedWorkerContext(env) }
