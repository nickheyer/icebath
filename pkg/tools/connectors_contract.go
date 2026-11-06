package tools

// TargetState is where one connection target stands in its operation
type TargetState string

const (
	TargetPending      TargetState = "pending"
	TargetInitiated    TargetState = "initiated"
	TargetConnected    TargetState = "connected"
	TargetSkipped      TargetState = "skipped"
	TargetFailed       TargetState = "failed"
	TargetExpired      TargetState = "expired"
	TargetNotConnected TargetState = "not_connected"
)

// ConnActor names who may cause a target transition
type ConnActor string

const (
	ActorUser           ConnActor = "user"
	ActorBackendWatcher ConnActor = "backend_watcher"
	ActorClock          ConnActor = "clock"
)

// SettleReason names why an operation froze its result
type SettleReason string

const (
	SettleAllResolved SettleReason = "all_resolved"
	SettleContinue    SettleReason = "continue"
	SettleDeadline    SettleReason = "deadline"
	SettleInterrupt   SettleReason = "interrupt"
)

// ConnectorKinds lists every target kind a connection operation carries
var ConnectorKinds = []string{"connector", "mcp", "plugin", "skill"}

// Reports whether the state ends the target, a failed catalog row counting as done for plugins and skills
func TargetResolves(kind string, state TargetState) bool {
	if state == TargetConnected || state == TargetSkipped {
		return true
	}
	return state == TargetFailed && (kind == "plugin" || kind == "skill")
}

type connEdge struct {
	kind string
	from TargetState
}

var connTransitions = buildConnTransitions()

// Builds the kind and state table of allowed moves with the one actor each move admits
func buildConnTransitions() map[connEdge]map[TargetState]ConnActor {
	t := map[connEdge]map[TargetState]ConnActor{
		{"connector", TargetPending}: {TargetInitiated: ActorBackendWatcher, TargetFailed: ActorBackendWatcher, TargetSkipped: ActorUser},
		{"connector", TargetInitiated}: {TargetConnected: ActorBackendWatcher, TargetFailed: ActorBackendWatcher,
			TargetExpired: ActorClock, TargetSkipped: ActorUser},
		{"connector", TargetFailed}:  {TargetInitiated: ActorUser, TargetSkipped: ActorUser},
		{"connector", TargetExpired}: {TargetInitiated: ActorUser, TargetSkipped: ActorUser},
		{"mcp", TargetPending}:       {TargetInitiated: ActorBackendWatcher, TargetFailed: ActorBackendWatcher, TargetSkipped: ActorUser},
		{"mcp", TargetInitiated}:     {TargetConnected: ActorBackendWatcher, TargetFailed: ActorBackendWatcher, TargetSkipped: ActorUser},
		{"mcp", TargetFailed}:        {TargetInitiated: ActorUser, TargetSkipped: ActorUser},
	}
	for _, kind := range []string{"plugin", "skill"} {
		for edge, moves := range t {
			if edge.kind != "mcp" {
				continue
			}
			cp := map[TargetState]ConnActor{}
			for to, actor := range moves {
				cp[to] = actor
			}
			t[connEdge{kind, edge.from}] = cp
		}
	}
	return t
}

// The only actor allowed to move a target of kind from current to to, empty when the move is illegal
func AllowedTransition(kind string, current, to TargetState) ConnActor {
	return connTransitions[connEdge{kind, current}][to]
}
