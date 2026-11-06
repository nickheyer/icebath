package agent

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	reasonHardStop        = "explicit stop requested"
	reasonNewMessage      = "user sent a new message"
	reasonUserInterrupt   = "user interrupt"
	reasonGatewayShutdown = "gateway shutdown"
)

// Interrupt categories that mean a human stopped the turn
var UserInterruptReasons = map[string]bool{reasonHardStop: true, reasonNewMessage: true, reasonUserInterrupt: true}

var interruptStdout io.Writer = os.Stdout

// One buffered steer submission and the ticket naming it, nil for a ticketless steer
type steerPart struct {
	ticket any
	text   string
}

// Text and parts handed out by the last steer drain so a requeue restores their tickets
type steerDrain struct {
	text  string
	parts []steerPart
}

// Interrupt request options matching the hard_cancel, tool_reason and require_generation keywords
type InterruptOptions struct {
	HardCancel        bool
	ToolReason        string
	RequireGeneration *int64
}

// Commit fence surface the hard stop drives before and after its claim edge
type compressionCommitCanceller interface {
	CommitInFlight() bool
	CancelBeforeCommit(cancelEvent any) bool
}

// Slug of the system producer behind the pending interrupt, empty for a human stop
func interruptIssuer(a *AIAgent) string {
	reason := a.toolInterruptReason
	if reason == "" || UserInterruptReasons[reason] {
		return ""
	}
	return strings.ReplaceAll(strings.TrimSpace(reason), " ", "_")
}

// Reason a tool call was skipped in words safe for tool output, from the recorded interrupt reason
func interruptSkipWording(a *AIAgent) string {
	reason := a.toolInterruptReason
	if reason == "" {
		return "Turn interrupted"
	}
	if UserInterruptReasons[reason] {
		return pystr.Capitalize(reason)
	}
	return "Turn aborted — " + strings.ReplaceAll(reason, "_", " ")
}

// Turn exit reason for an API call cut short by an interrupt, naming a system issuer when present
func interruptedDuringAPICallReason(a *AIAgent) string {
	if issuer := interruptIssuer(a); issuer != "" {
		return "interrupted_during_api_call(" + issuer + ")"
	}
	return "interrupted_during_api_call"
}

// Cancels the fence before commit when its in flight state matches, logging a failure at debug
func fenceCancelBeforeCommit(fence any, whenInFlight bool, failureLog string) {
	f, ok := fence.(compressionCommitCanceller)
	if fence == nil || !ok || f.CommitInFlight() != whenInFlight {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			runAgentLog().Debug(failureLog, "error", r)
		}
	}()
	f.CancelBeforeCommit(nil)
}

// Locks the mutex when present so lockless test agents run unlocked
func icLock(mu *sync.Mutex) func() {
	if mu == nil {
		return func() {}
	}
	mu.Lock()
	return mu.Unlock
}

// Buffered steers as ticket and text pairs, one ticketless part when another writer set the buffer
func (a *AIAgent) steerParts() []steerPart {
	texts := make([]string, len(a.pendingSteerParts))
	for i, p := range a.pendingSteerParts {
		texts[i] = p.text
	}
	if strings.Join(texts, "\n") != a.pendingSteer {
		if a.pendingSteer != "" {
			return []steerPart{{nil, a.pendingSteer}}
		}
		return nil
	}
	return append([]steerPart(nil), a.pendingSteerParts...)
}

// Stores the steer parts and their newline join as the pending steer text
func (a *AIAgent) setSteerParts(parts []steerPart) {
	a.pendingSteerParts = parts
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = p.text
	}
	a.pendingSteer = strings.Join(texts, "\n")
}

// Ticket to consumed map for ticketed steers that left the buffer, created on first use
func (a *AIAgent) steerFates() map[any]bool {
	if a.steerTicketFates == nil {
		a.steerTicketFates = map[any]bool{}
	}
	return a.steerTicketFates
}

// Puts drained steer text back, restoring the last drain's tickets when the text matches it
func requeueDrainedSteer(a *AIAgent, steerText string) {
	defer icLock(a.pendingSteerLock)()
	drain := a.steerLastDrain
	a.steerLastDrain = nil
	var parts []steerPart
	if drain != nil && drain.text == steerText && len(drain.parts) > 0 {
		parts = drain.parts
	} else {
		parts = []steerPart{{nil, steerText}}
	}
	for _, p := range parts {
		if p.ticket != nil {
			delete(a.steerFates(), p.ticket)
		}
	}
	a.setSteerParts(append(a.steerParts(), parts...))
}

// Puts drained steer text back so the fallback delivers it as a next turn user message
func requeuePendingSteer(a *AIAgent, steerText string) { requeueDrainedSteer(a, steerText) }

// Native turn control of the codex app server route, which owns its model and tool loop
type codexTurnControl interface {
	RequestInterrupt()
	RequestSteer(text string) (bool, error)
}

var codexTurnControlFor = func(a *AIAgent) codexTurnControl {
	if a.codexSession == nil {
		return nil
	}
	return a.codexSession
}

// Codex turn control when the agent runs on the codex app server route, nil otherwise
func (a *AIAgent) icCodexSession() codexTurnControl {
	if a.apiMode != "codex_app_server" {
		return nil
	}
	return codexTurnControlFor(a)
}

// Shuts the registered in flight request sockets, logging a failure at debug
func (a *AIAgent) icAbortActiveRequest(reason, failureLog string) {
	abort := a.activeRequestAbort
	if abort == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			runAgentLog().Debug(failureLog, "error", r)
		}
	}()
	abort(reason)
}

// Fans the tool interrupt bit out to every concurrent tool worker of this agent
func (a *AIAgent) icSignalToolWorkers(active bool, reason string) {
	for _, id := range a.toolWorkerIDs() {
		tools.SetInterrupt(id, active, reason)
	}
}

// Snapshot of the concurrent tool worker ids under their lock, nil without a tracker
func (a *AIAgent) toolWorkerIDs() []tools.WorkerID {
	if a.toolWorkerThreads == nil || a.toolWorkerThreadsLock == nil {
		return nil
	}
	a.toolWorkerThreadsLock.Lock()
	defer a.toolWorkerThreadsLock.Unlock()
	ids := make([]tools.WorkerID, 0, len(a.toolWorkerThreads))
	for id := range a.toolWorkerThreads {
		ids = append(ids, tools.WorkerID(id))
	}
	return ids
}

// Soft interrupt of the current tool calling loop with an optional next message
func (a *AIAgent) Interrupt(message string) { a.InterruptWith(message, InterruptOptions{}) }

// Explicit stop that compression honors even while ordinary interrupts are masked
func (a *AIAgent) HardInterrupt(message, toolReason string) {
	a.InterruptWith(message, InterruptOptions{HardCancel: true, ToolReason: toolReason})
}

// Requests the interrupt, false when a generation claim lost to resumed turn activity
func (a *AIAgent) InterruptWith(message string, o InterruptOptions) bool {
	if o.RequireGeneration != nil {
		mu := a.livenessActivityLock()
		mu.Lock()
		if a.turnLivenessActivityGeneration != *o.RequireGeneration {
			mu.Unlock()
			return false
		}
		a.turnLivenessAbortClaim = *o.RequireGeneration
		mu.Unlock()
	}
	toolInterruptReason := o.ToolReason
	if toolInterruptReason == "" {
		switch {
		case o.HardCancel:
			toolInterruptReason = reasonHardStop
		case message != "":
			toolInterruptReason = reasonNewMessage
		default:
			toolInterruptReason = reasonUserInterrupt
		}
	}
	publish := func() {
		if !(o.ToolReason == reasonGatewayShutdown && a.interruptRequested && UserInterruptReasons[a.toolInterruptReason]) {
			a.interruptMessage = message
			a.toolInterruptReason = toolInterruptReason
		}
		a.interruptRequested = true
		a.turnUserIntervened = true
		mode := "soft"
		if o.HardCancel {
			mode = "hard"
		}
		runAgentLog().Info(fmt.Sprintf("Interrupt requested (%s): %s", mode, toolInterruptReason))
		if o.HardCancel && a.hardInterruptRequested != nil {
			a.hardInterruptRequested.Set()
		}
	}
	fence := func() any {
		if o.HardCancel {
			return a.activeCompressionCommitFence
		}
		return nil
	}
	if !a.publishInterruptUnderRedirectLock(o, publish, fence) {
		return false
	}
	if s := a.icCodexSession(); s != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					runAgentLog().Debug("Failed to interrupt Codex app-server turn", "error", r)
				}
			}()
			s.RequestInterrupt()
		}()
	}
	a.icAbortActiveRequest("interrupt_abort", "Failed to abort active inline request")
	a.signalExecutionThread(toolInterruptReason)
	a.icSignalToolWorkers(true, toolInterruptReason)
	for _, child := range a.activeChildrenSnapshot() {
		propagateInterruptToChild(child, message, toolInterruptReason, o.HardCancel)
	}
	if !a.quietMode {
		suffix := ""
		if message != "" {
			if r := []rune(message); len(r) > 40 {
				suffix = ": '" + string(r[:40]) + "...'"
			} else {
				suffix = ": '" + message + "'"
			}
		}
		fmt.Fprintln(interruptStdout, "\n⚡ Interrupt requested"+suffix)
	}
	return true
}

// Runs the fence wait, the claim edge publication and the fence admission under the redirect lock
func (a *AIAgent) publishInterruptUnderRedirectLock(o InterruptOptions, publish func(), fence func() any) bool {
	defer icLock(a.pendingRedirectLock)()
	fenceCancelBeforeCommit(fence(), true, "Compression hard-cancel fence wait failed")
	if o.RequireGeneration == nil {
		publish()
	} else {
		mu := a.livenessActivityLock()
		mu.Lock()
		claim, ok := a.turnLivenessAbortClaim.(int64)
		if !ok || claim != *o.RequireGeneration {
			mu.Unlock()
			return false
		}
		a.turnLivenessAbortClaim = nil
		publish()
		mu.Unlock()
	}
	fenceCancelBeforeCommit(fence(), false, "Compression hard-cancel fence admission failed")
	a.pendingRedirect = ""
	return true
}

// Sets the tool interrupt on the bound execution thread, else defers it until the thread binds
func (a *AIAgent) signalExecutionThread(reason string) {
	defer icLock(a.pendingRedirectLock)()
	if a.executionThreadID != 0 {
		tools.SetInterrupt(tools.WorkerID(a.executionThreadID), true, reason)
		a.interruptThreadSignalPending = false
	} else {
		a.interruptThreadSignalPending = true
	}
}

// Running child agents snapshotted under their lock
func (a *AIAgent) activeChildrenSnapshot() []*AIAgent {
	defer icLock(a.activeChildrenLock)()
	return append([]*AIAgent(nil), a.activeChildren...)
}

// Forwards the interrupt to a delegated child, logging a failure at debug
func propagateInterruptToChild(child *AIAgent, message, toolReason string, hard bool) {
	defer func() {
		if r := recover(); r != nil {
			runAgentLog().Debug(fmt.Sprintf("Failed to propagate interrupt to child agent: %v", r))
		}
	}()
	if hard {
		RequestHardInterrupt(child, message, toolReason)
	} else {
		child.Interrupt(message)
	}
}

// Clears the interrupt and tool signal, keeping the pending steer
func (a *AIAgent) clearInterrupt(preserveRedirect bool) bool {
	return a.clearInterruptWith(preserveRedirect, false)
}

// Clears the interrupt request and tool signal, a hard cancel also drops the unconsumed steer
func (a *AIAgent) clearInterruptWith(preserveRedirect, hardCancel bool) bool {
	unlock := icLock(a.pendingRedirectLock)
	if preserveRedirect && a.pendingRedirect == "" {
		unlock()
		return false
	}
	a.interruptRequested = false
	a.interruptMessage = ""
	a.toolInterruptReason = ""
	if a.hardInterruptRequested != nil {
		a.hardInterruptRequested.Clear()
	}
	if !preserveRedirect {
		a.pendingRedirect = ""
	}
	unlock()
	a.interruptThreadSignalPending = false
	if a.executionThreadID != 0 {
		tools.SetInterrupt(tools.WorkerID(a.executionThreadID), false, "")
	}
	a.icSignalToolWorkers(false, "")
	if hardCancel {
		defer icLock(a.pendingSteerLock)()
		for _, p := range a.steerParts() {
			if p.ticket != nil {
				a.steerFates()[p.ticket] = false
			}
		}
		a.setSteerParts(nil)
	}
	return true
}

// Queues user text for delivery as its own user row after the current tool batch, false for blank text
func (a *AIAgent) Steer(text string) bool { return a.SteerTicket(text, nil) }

// Queues steer text under a ticket that WithdrawSteer can settle later
func (a *AIAgent) SteerTicket(text string, ticket any) bool {
	cleaned := pystr.Strip(text)
	if cleaned == "" {
		return false
	}
	defer icLock(a.pendingSteerLock)()
	a.setSteerParts(append(a.steerParts(), steerPart{ticket, cleaned}))
	a.turnUserIntervened = true
	return true
}

// Redirects the active model request with a correction, degrading to a steer during tool execution
func (a *AIAgent) Redirect(text string) bool {
	cleaned := pystr.Strip(text)
	if cleaned == "" {
		return false
	}
	if s := a.icCodexSession(); s != nil {
		unlock := icLock(a.pendingRedirectLock)
		interrupted := a.interruptRequested
		unlock()
		if interrupted {
			return false
		}
		accepted, err := s.RequestSteer(cleaned)
		if err != nil {
			runAgentLog().Debug("Codex app-server turn/steer failed", "error", err)
			return false
		}
		if accepted {
			a.turnUserIntervened = true
		}
		return accepted
	}
	if a.executingTools {
		accepted := a.Steer(cleaned)
		if accepted {
			for _, id := range a.toolWorkerIDs() {
				tools.RequestYield(id)
			}
		}
		return accepted
	}
	unlock := icLock(a.pendingRedirectLock)
	if a.modelRequestActive == nil || !a.modelRequestActive.IsSet() {
		unlock()
		return false
	}
	existing := a.pendingRedirect
	if a.interruptRequested && existing == "" {
		unlock()
		return false
	}
	if existing != "" {
		a.pendingRedirect = existing + "\n\n[Additional user correction]\n" + cleaned
	} else {
		a.pendingRedirect = cleaned
	}
	a.interruptRequested = true
	a.interruptMessage = ""
	a.turnUserIntervened = true
	unlock()
	a.signalExecutionThread("")
	a.icAbortActiveRequest("redirect_abort", "Failed to abort request for redirect")
	return true
}

// True while an active turn redirect waits to be applied
func (a *AIAgent) hasPendingRedirect() bool {
	defer icLock(a.pendingRedirectLock)()
	return a.pendingRedirect != ""
}

// Pending active turn correction text, cleared on read
func (a *AIAgent) drainPendingRedirect() string {
	defer icLock(a.pendingRedirectLock)()
	text := a.pendingRedirect
	a.pendingRedirect = ""
	return text
}

// Pending steer text cleared on read, marking each ticketed part consumed
func (a *AIAgent) drainPendingSteer() string {
	defer icLock(a.pendingSteerLock)()
	text := a.pendingSteer
	parts := a.steerParts()
	a.setSteerParts(nil)
	for _, p := range parts {
		if p.ticket != nil {
			a.steerFates()[p.ticket] = true
		}
	}
	a.steerLastDrain = &steerDrain{text: text, parts: parts}
	return text
}

// Ticketless steers for the next user turn, keeping every ticketed steer buffered
func (a *AIAgent) drainLeftoverSteer() string {
	defer icLock(a.pendingSteerLock)()
	parts := a.steerParts()
	var kept []steerPart
	var texts []string
	for _, p := range parts {
		if p.ticket != nil {
			kept = append(kept, p)
		} else {
			texts = append(texts, p.text)
		}
	}
	a.setSteerParts(kept)
	return strings.Join(texts, "\n")
}

// Settles one ticketed steer, true when no drain handed it to the turn
func (a *AIAgent) WithdrawSteer(ticket any) bool {
	if ticket == nil {
		return false
	}
	defer icLock(a.pendingSteerLock)()
	parts := a.steerParts()
	var kept []steerPart
	for _, p := range parts {
		if p.ticket != ticket {
			kept = append(kept, p)
		}
	}
	consumed, known := a.steerFates()[ticket]
	delete(a.steerFates(), ticket)
	if len(kept) == len(parts) {
		return known && !consumed
	}
	a.setSteerParts(kept)
	return true
}
