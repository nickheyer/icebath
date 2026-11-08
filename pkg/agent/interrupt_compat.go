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
	if toolReason != nil {
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

// Hard stop of an agent with an optional message and tool reason, empty strings standing for none
func RequestHardInterrupt(a *AIAgent, reason, toolReason string) bool {
	var message, category *string
	if reason != "" {
		message = &reason
	}
	if toolReason != "" {
		category = &toolReason
	}
	return RequestHardInterruptOf(a, message, category)
}
