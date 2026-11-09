package agent

// Explicit stop ABI of new agents with the fixed tool reason category
type hardInterruptWithReason interface {
	HardInterrupt(message, toolReason string)
}

// Explicit stop ABI of agents whose hard stop takes only the message
type hardInterruptMessageOnly interface{ HardInterrupt(message string) }

// Legacy interrupt ABI that also accepts the tool reason category
type interruptWithReason interface {
	Interrupt(message, toolReason string)
}

// Legacy interrupt ABI of third party agents and old doubles
type interruptMessageOnly interface{ Interrupt(message string) }

// Reports whether the stop callable the agent exposes takes the tool reason keyword
func acceptsToolReason(agent any) bool {
	switch agent.(type) {
	case hardInterruptWithReason, interruptWithReason:
		return true
	}
	return false
}

// Hard stop of any agent value, falling back to the legacy interrupt and false when neither exists
func RequestHardInterruptOf(agent any, message, toolReason *string) bool {
	if agent == nil {
		return false
	}
	if a, ok := agent.(*AIAgent); ok && a == nil {
		return false
	}
	msg := ""
	if message != nil {
		msg = *message
	}
	reason := ""
	if toolReason != nil && acceptsToolReason(agent) {
		reason = *toolReason
	}
	switch target := agent.(type) {
	case hardInterruptWithReason:
		target.HardInterrupt(msg, reason)
	case hardInterruptMessageOnly:
		target.HardInterrupt(msg)
	case interruptWithReason:
		target.Interrupt(msg, reason)
	case interruptMessageOnly:
		target.Interrupt(msg)
	default:
		return false
	}
	return true
}

// Hard stop of an AIAgent, empty strings standing for the absent message and tool reason, false for a nil agent
func RequestHardInterrupt(agent *AIAgent, message, toolReason string) bool {
	if agent == nil {
		return false
	}
	var msg, reason *string
	if message != "" {
		msg = &message
	}
	if toolReason != "" {
		reason = &toolReason
	}
	return RequestHardInterruptOf(agent, msg, reason)
}
