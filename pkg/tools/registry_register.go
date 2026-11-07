package tools

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// ErrRegistrationRejected marks a registration the registry refused and already logged at error level
var ErrRegistrationRejected = errors.New("tool registration rejected")

type overrideDeniedError struct{ owner, name string }

// Renders the operator opt in refusal with the owner and tool name quoted like Python repr
func (e *overrideDeniedError) Error() string {
	return fmt.Sprintf("Plugin module %s cannot override built-in tool %s without operator opt-in (allow_tool_override).",
		pyrepr.StrRepr(e.owner), pyrepr.StrRepr(e.name))
}

// Matches ErrOverrideDenied so callers keep telling denials from rejections
func (e *overrideDeniedError) Is(target error) bool { return target == ErrOverrideDenied }

type deregisterDeniedError struct{ msg string }

func (e *deregisterDeniedError) Error() string { return e.msg }

// Matches ErrOverrideDenied so a refused removal reads as a permission failure
func (e *deregisterDeniedError) Is(target error) bool { return target == ErrOverrideDenied }

// Refuses a missing schema or a non object parameters value before anything is stored
func validateRegistrationSchema(d Def) error {
	if d.Schema == nil {
		return fmt.Errorf("Tool %s: schema must be a dict, got NoneType", pyrepr.StrRepr(d.Name))
	}
	if p, ok := d.Schema["parameters"]; ok && p != nil {
		if _, isMap := p.(map[string]any); !isMap {
			return fmt.Errorf("Tool %s: schema['parameters'] must be an object (JSON Schema dict), got %s", pyrepr.StrRepr(d.Name), pyrepr.TypeName(p))
		}
	}
	return nil
}

// Logs and returns the refusal of a plugin shadowing a global tool without override
func rejectGlobalShadow(owner, name string) error {
	slog.Error(fmt.Sprintf("Tool registration REJECTED: plugin %s attempted to shadow global tool %s without override=True",
		pyrepr.StrRepr(owner), pyrepr.StrRepr(name)))
	return fmt.Errorf("%w: plugin %s would shadow global tool %s", ErrRegistrationRejected, pyrepr.StrRepr(owner), pyrepr.StrRepr(name))
}

// Decides a cross toolset replacement, logging the refusal, the denial or the audited override
func crossToolsetVerdict(d Def, owner, existingToolset string, denied bool) error {
	if d.Override {
		if denied {
			slog.Error(fmt.Sprintf("Tool registration REJECTED: plugin %s attempted to override built-in tool %s (existing toolset %s) without operator opt-in. "+
				"Set plugins.entries.<plugin_id>.allow_tool_override: true in config.yaml to allow it.",
				pyrepr.StrRepr(owner), pyrepr.StrRepr(d.Name), pyrepr.StrRepr(existingToolset)))
			return &overrideDeniedError{owner: owner, name: d.Name}
		}
		slog.Info(fmt.Sprintf("Tool '%s': toolset '%s' overriding existing toolset '%s' (override=True opt-in)", d.Name, d.Toolset, existingToolset))
		return nil
	}
	slog.Error(fmt.Sprintf("Tool registration REJECTED: '%s' (toolset '%s') would shadow existing tool from toolset '%s'. "+
		"Pass override=True to register() if the replacement is intentional, or deregister the existing tool first.", d.Name, d.Toolset, existingToolset))
	return fmt.Errorf("%w: %s (toolset %s) would shadow existing tool from toolset %s", ErrRegistrationRejected,
		pyrepr.StrRepr(d.Name), pyrepr.StrRepr(d.Toolset), pyrepr.StrRepr(existingToolset))
}

// Logs and returns the refusal of a plugin removing a tool it neither owns nor may override
func rejectForeignDeregister(caller, name, toolset string) error {
	slog.Error(fmt.Sprintf("Tool deregistration REJECTED: plugin %s attempted to remove tool %s (toolset %s) it does not own, without operator opt-in. "+
		"Set plugins.entries.%s.allow_tool_override: true in config.yaml to allow it.",
		pyrepr.StrRepr(caller), pyrepr.StrRepr(name), pyrepr.StrRepr(toolset), caller))
	return &deregisterDeniedError{msg: fmt.Sprintf("Plugin module %s cannot deregister tool %s (toolset %s) without operator opt-in (allow_tool_override).",
		pyrepr.StrRepr(caller), pyrepr.StrRepr(name), pyrepr.StrRepr(toolset))}
}

// Resolves the scope a plugin caller acts in, refusing a scope outside the profiles it is bound to
func (e *registryExt) callerScopeLocked(callerOwner, scope string) (string, error) {
	scopes := e.moduleScopes[callerOwner]
	if len(scopes) == 0 {
		return scope, nil
	}
	if scope != "" {
		if !scopes[scope] {
			return "", &deregisterDeniedError{msg: fmt.Sprintf("Plugin module %s cannot deregister tools outside its own profile scope.", pyrepr.StrRepr(callerOwner))}
		}
		return scope, nil
	}
	if len(scopes) == 1 {
		for s := range scopes {
			return s, nil
		}
	}
	return "", &deregisterDeniedError{msg: fmt.Sprintf("Plugin module %s is active in multiple profiles and cannot register outside one of those scopes.", pyrepr.StrRepr(callerOwner))}
}
