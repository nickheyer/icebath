package server

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// Helper models of the session for a step of its turn, following the main model the step runs on
func (a *Agent) helpers(r *run, settings *config.Config, main provider.Route) *auxiliary.Client {
	r.mu.Lock()
	chosen := r.provider != ""
	r.mu.Unlock()
	client := &auxiliary.Client{
		Tasks:       a.auxiliary,
		Config:      settings,
		Providers:   a.providers,
		Transports:  a.transports,
		Credentials: a.credentials,
		State:       a.learned,
		Main:        main,
		Chosen:      chosen || settings.GetString(agentconfig.Provider) != "",
		ImageInput:  settings.GetString(agentconfig.ImageInput),
		Fallbacks:   a.chain(settings),
		Used: func(task string, route provider.Route, usage provider.Usage) {
			a.record(r, route, task, &usage, 0)
		},
	}
	switch settings.GetString(agentconfig.SupportsVision) {
	case agentconfig.Sees:
		client.MainSees = new(true)
	case agentconfig.Blind:
		client.MainSees = new(false)
	}
	return client
}

// Text as a sentence: its first letter in capitals, icebath left in lower case, and a period at its end unless it ends a sentence already;
// empty for none
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if first, size := utf8.DecodeRuneInString(text); !strings.HasPrefix(text, "icebath") {
		text = string(unicode.ToUpper(first)) + text[size:]
	}
	if !strings.ContainsAny(text[len(text)-1:], ".!?") {
		text += "."
	}
	return text
}
