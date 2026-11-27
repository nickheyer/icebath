package config

import hooksconfig "github.com/nickheyer/icebath/internal/hooks/config"

const (
	Key = hooksconfig.Key + ".shell"
	// Commands to run on agent events, a list of definitions by event name
	Events = Key + ".events"
	// Every hook the events name runs without asking first
	AutoAccept = Key + ".auto_accept"
	// Addresses agent events are posted to, a list of targets
	Outbound = Key + ".outbound"
)
