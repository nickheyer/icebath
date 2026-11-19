package gateway

import (
	"sort"

	"github.com/nickheyer/icebath/pkg/tuihost"
)

// SetLocalVoiceMode sets and persists a local session's voice mode on the running owner, nil when no owner here holds it
func SetLocalVoiceMode(sessionID any, mode string) (*bool, error) {
	runner := ActiveAPIRunner()
	id, ok := sessionID.(string)
	if runner == nil || !ok || id == "" {
		return nil, nil
	}
	for _, a := range AllAuthorities(runner) {
		live, held := a.live(id)
		if !held {
			continue
		}
		modes, isVoiceRunner := runner.(voiceModeRunner)
		if !isVoiceRunner {
			return nil, runnerLacks("report the voice mode")
		}
		modes.StoreVoiceMode(live.Source, mode)
		LocalVoiceTransition(live.Source, mode)
		held = true
		return &held, nil
	}
	return nil, nil
}

// LocalVoiceMode is the stored voice mode of a local session the running owner holds, held false when none here holds it
func LocalVoiceMode(sessionID string) (mode string, held bool, err error) {
	runner := ActiveAPIRunner()
	if runner == nil || sessionID == "" {
		return "", false, nil
	}
	for _, a := range AllAuthorities(runner) {
		live, ok := a.live(sessionID)
		if !ok {
			continue
		}
		if live.Source == nil {
			return "off", true, nil
		}
		modes, isVoiceRunner := runner.(voiceModeRunner)
		if !isVoiceRunner {
			return "", true, runnerLacks("derive the voice key of a source")
		}
		return modes.VoiceModeFor(live.Source), true, nil
	}
	return "", false, nil
}

// LocalVoiceViewers are the event transports of the windows viewing a local session, sorted by subscription id
func LocalVoiceViewers(sessionID string) []tuihost.Transport {
	runner := ActiveAPIRunner()
	if runner == nil || sessionID == "" {
		return nil
	}
	for _, a := range AllAuthorities(runner) {
		a.mu.Lock()
		live := a.Sessions[sessionID]
		if live == nil {
			a.mu.Unlock()
			continue
		}
		keys := make([]string, 0, len(live.Subscribers))
		for key := range live.Subscribers {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var out []tuihost.Transport
		for _, key := range keys {
			if transport := a.Events[live.Subscribers[key].TransportID]; transport != nil {
				out = append(out, transport)
			}
		}
		a.mu.Unlock()
		return out
	}
	return nil
}
