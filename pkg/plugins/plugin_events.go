package plugins

import (
	"fmt"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Prefix every plugin event name starts with, core owning every other name
const PluginEventPrefix = "plugin."

var (
	pluginIDPattern    = mustPyRe(`^[a-z0-9_-]{1,64}$`)
	pluginEventPattern = mustPyRe(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)
)

// Reports whether a Python pattern matches at the start of the text
func pyReMatch(re *regexp2.Regexp, text string) bool {
	m, err := re.FindStringMatch(text)
	return err == nil && m != nil && m.RuneIndex == 0
}

// Error a ValueError raised by the plugin event naming carries
type PluginEventNameError struct{ Msg string }

func (e *PluginEventNameError) Error() string      { return e.Msg }
func (e *PluginEventNameError) PyTypeName() string { return "ValueError" }

// Error a TypeError raised for a plugin event payload that is not a dict carries
type PluginEventPayloadError struct{ Msg string }

func (e *PluginEventPayloadError) Error() string      { return e.Msg }
func (e *PluginEventPayloadError) PyTypeName() string { return "TypeError" }

// Wire name plugin dot id dot event, a ValueError for an id or event that cannot form one
func PluginEventName(pluginID, event string) (string, error) {
	if !pyReMatch(pluginIDPattern, pluginID) {
		return "", &PluginEventNameError{Msg: fmt.Sprintf("invalid plugin id %s: expected [a-z0-9_-]{1,64}", pyrepr.Repr(pluginID))}
	}
	if utf8.RuneCountInString(event) > 128 || !pyReMatch(pluginEventPattern, event) {
		return "", &PluginEventNameError{Msg: fmt.Sprintf("invalid plugin event %s: expected dot-separated segments of [A-Za-z0-9_-] "+
			"(the plugin id already namespaces the name)", pyrepr.Repr(event))}
	}
	return PluginEventPrefix + pluginID + "." + event, nil
}

// Emits the namespaced plugin event to every connected client, a logged no op without a gateway here
func BroadcastPluginEvent(pluginID, event string, payload any) error {
	var body map[string]any
	if payload != nil {
		dict, ok := payload.(map[string]any)
		if !ok {
			return &PluginEventPayloadError{Msg: "plugin event payload must be a dict or None, got " + pyrepr.TypeName(payload)}
		}
		body = dict
	}
	name, err := PluginEventName(pluginID, event)
	if err != nil {
		return err
	}
	s := installedServices()
	if s == nil || s.BroadcastGlobalEvent == nil {
		logWarn(fmt.Sprintf("plugin event %s dropped: no tui_gateway in this process", name),
			"error", &ServiceMissingError{Name: "BroadcastGlobalEvent"})
		return nil
	}
	copied := make(map[string]any, len(body))
	for k, v := range body {
		copied[k] = v
	}
	s.BroadcastGlobalEvent(name, copied)
	return nil
}
