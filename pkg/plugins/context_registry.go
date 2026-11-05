package plugins

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
)

// Replacement coordinator slot of one key in one manager mapping
type mappingSlot struct {
	mapping uintptr
	key     string
}

// Optional setting of a system prompt section registration
type SectionOption func(*SystemPromptSection)

// Places the section at a prompt position
func WithSectionPosition(position string) SectionOption {
	return func(s *SystemPromptSection) { s.Position = position }
}

// Caps the rendered section length in characters
func WithSectionMaxChars(maxChars int) SectionOption {
	return func(s *SystemPromptSection) { s.MaxChars = maxChars }
}

// Marks this load as timed out so every later registrar, subscribe and unload callback is ignored
func (c *Context) abandonLoad() { c.loadAbandoned.Store(true) }

// Reports and warns when a registrar runs after this context's load was abandoned
func (c *Context) ignoredAfterAbandonedLoad(method string) bool {
	if !c.loadAbandoned.Load() {
		return false
	}
	logWarn(fmt.Sprintf("Plugin '%s' called %s() after its load timed out; ignored", c.manifest.Name, method))
	return true
}

// Error for a malformed registration completing the sentence tried to register
func (c *Context) refuse(what string) error {
	return fmt.Errorf("Plugin '%s' tried to register %s.", c.manifest.Name, what)
}

// Stores an entry under a key in a manager mapping and leases the slot so unload restores the predecessor
func trackMappingEntry[V comparable](c *Context, kind, key string, mapping map[string]V, entry V, previous *V) *Registration {
	m := c.manager
	m.mu.Lock()
	prev := mapping[key]
	if previous != nil {
		prev = *previous
	}
	mapping[key] = entry
	m.mu.Unlock()
	slot := mappingSlot{mapping: reflect.ValueOf(mapping).Pointer(), key: key}
	return c.trackReplacement(kind, key, slot, entry, prev, func(replacement any) error {
		r, _ := replacement.(V)
		m.mu.Lock()
		restoreMapping(mapping, key, entry, r)
		m.mu.Unlock()
		return nil
	})
}

// Leases one generation of a replaceable manager slot so unload restores the predecessor while current
func (c *Context) trackReplacement(kind, key string, slot, current, previous any, restore func(replacement any) error) *Registration {
	lease := replacementCoordinator.Acquire(slot, current, previous, restore, nil)
	return c.track(kind, key, lease.Dispose, false)
}

// Stores and leases a manager mapping entry then logs the registration at debug
func registerEntry[V comparable](c *Context, kind, key string, mapping map[string]V, entry V, previous *V, logMsg string) *Registration {
	handle := trackMappingEntry(c, kind, key, mapping, entry, previous)
	logDebug(logMsg)
	return handle
}

// Normalizes section content to text or a session info callback
func sectionContent(content any) (string, func(map[string]any) (any, error), bool) {
	switch v := content.(type) {
	case string:
		return v, nil, true
	case func(map[string]any) (any, error):
		return "", v, v != nil
	case func(map[string]any) (string, error):
		return "", func(info map[string]any) (any, error) { return v(info) }, v != nil
	case func(map[string]any) string:
		return "", func(info map[string]any) (any, error) { return v(info), nil }, v != nil
	case func(map[string]any) any:
		return "", func(info map[string]any) (any, error) { return v(info), nil }, v != nil
	}
	return "", nil, false
}

// Registers bounded context frozen into each new session prompt, from text or a session info callback
func (c *Context) RegisterSystemPromptSection(id string, content any, opts ...SectionOption) (*Registration, error) {
	if c.ignoredAfterAbandonedLoad("register_system_prompt_section") {
		return nil, nil
	}
	if !IsValidSystemPromptSectionID(id) {
		return nil, errors.New("system prompt section id must be 1-128 lowercase characters using letters, numbers, '.', '_', or '-'")
	}
	text, fn, ok := sectionContent(content)
	if !ok {
		return nil, errors.New("system prompt section content must be a string or callable")
	}
	section := &SystemPromptSection{ID: id, Content: text, ContentFunc: fn, Position: "after_memory",
		MaxChars: DefaultSystemPromptSectionMaxChars, Plugin: c.PluginID()}
	for _, opt := range opts {
		opt(section)
	}
	if !SystemPromptSectionPositions[section.Position] {
		return nil, errors.New("system prompt section position must be one of: " + strings.Join(sortedSet(SystemPromptSectionPositions), ", "))
	}
	if section.MaxChars <= 0 || section.MaxChars > MaxSystemPromptSectionChars {
		return nil, fmt.Errorf("system prompt section max_chars must be between 1 and %d", MaxSystemPromptSectionChars)
	}
	c.manager.mu.RLock()
	existing := c.manager.systemPromptSections[id]
	c.manager.mu.RUnlock()
	if existing != nil {
		return nil, fmt.Errorf("system prompt section %s is already registered by plugin %s", config.PyRepr(id), config.PyRepr(existing.Plugin))
	}
	return registerEntry(c, "system_prompt_section", id, c.manager.systemPromptSections, section, &existing,
		fmt.Sprintf("Plugin %s registered system prompt section: %s", c.manifest.Name, id)), nil
}

// Publishes a bare event under this plugin's namespace and returns the subscriber count scheduled
func (c *Context) Emit(ctx context.Context, event string, payload map[string]any) (int, error) {
	key := c.PluginID()
	if event == "" {
		logWarn(fmt.Sprintf("Plugin '%s' tried to emit an invalid event name %s", key, config.PyRepr(event)))
		return 0, fmt.Errorf("Plugin '%s' emit() requires a non-empty event name", key)
	}
	if strings.Contains(event, ":") {
		logWarn(fmt.Sprintf("Plugin '%s' tried to emit namespaced/reserved event '%s' — a plugin may only emit bare event names under its own '%s:' namespace (the '%s:' prefix is reserved for core, and foreign namespaces are forbidden)",
			key, event, key, CoreEventNamespace))
		return 0, fmt.Errorf("Plugin '%s' may not emit '%s': emit only the bare event name; the namespace is forced to '%s:' and the '%s:' prefix is reserved for core",
			key, event, key, CoreEventNamespace)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return c.manager.dispatchEvent(ctx, key+":"+event, payload), nil
}

// Subscribes to a fully qualified plugin event, owner tagged so a failed load removes the callback
func (c *Context) Subscribe(event string, fn EventFunc) error {
	if c.ignoredAfterAbandonedLoad("subscribe") {
		return nil
	}
	if event == "" {
		return fmt.Errorf("Plugin '%s' subscribe() requires a non-empty event name", c.manifest.Name)
	}
	if err := c.manager.subscribeEvent(c.PluginID(), event, fn); err != nil {
		return err
	}
	logDebug(fmt.Sprintf("Plugin %s subscribed to event: %s", c.manifest.Name, event))
	return nil
}
