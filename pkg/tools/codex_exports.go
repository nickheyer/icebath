package tools

import "context"

// DelegatedChildEnvMarker carries the delegate child fence across a spawn, the exported face for the codex app server
const DelegatedChildEnvMarker = delegatedChildEnvMarker

// Worker identity keys scrubbed from every descendant, the exported face for the codex app server spawn
func KanbanEnvKeys() []string { return append([]string(nil), kanbanEnvKeys...) }

// False when a kanban identity is present but this execution does not own it, exported for the codex spawn
func IsDispatcherOwnedWorkerContext(env *Env) bool { return isDispatcherOwnedWorkerContext(env) }

// Session asker as the codex approval prompt answering once, session, always or deny, nil without an asker
func CodexApprovalPrompt(env *Env) func(command, description string, allowPermanent bool) (string, error) {
	if env == nil || env.Ask == nil {
		return nil
	}
	return func(command, description string, allowPermanent bool) (string, error) {
		choice := promptDangerousApproval(context.Background(), env, promptOptions{command: command, description: description, allowPermanent: allowPermanent, allowSession: true})
		return choice.value, nil
	}
}

// Swaps the launch frozen yolo flag and returns its restore, the module attribute codex tests patch
func SwapYoloModeFrozen(on bool) func() {
	prev := yoloModeFrozen
	yoloModeFrozen = on
	return func() { yoloModeFrozen = prev }
}
