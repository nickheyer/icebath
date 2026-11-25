package tools

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/nickheyer/icebath/pkg/errchain"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// ErrRegistrationRejected marks a registration the registry refused and already logged at error level
var ErrRegistrationRejected = errors.New("tool registration rejected")

type overrideDeniedError struct{ owner, name string }

// Renders the operator opt in refusal with the owner and tool name quoted
func (e *overrideDeniedError) Error() string {
	return fmt.Sprintf("Plugin module %s cannot override built-in tool %s without operator opt-in (allow_tool_override).",
		strconv.Quote(e.owner), strconv.Quote(e.name))
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
		return fmt.Errorf("Tool %s: schema must be a mapping, got nothing", strconv.Quote(d.Name))
	}
	if p, ok := d.Schema.Get("parameters"); ok && p != nil {
		if _, isMap := jsonutil.AsMap(p); !isMap {
			return fmt.Errorf("Tool %s: schema[\"parameters\"] must be a mapping (a JSON Schema object), got %s", strconv.Quote(d.Name), errchain.Kind(p))
		}
	}
	return nil
}

// Logs and returns the refusal of a plugin shadowing a global tool without override
func rejectGlobalShadow(owner, name string) error {
	pkgLog().Error(fmt.Sprintf("Tool registration REJECTED: plugin %s attempted to shadow global tool %s without override=True",
		strconv.Quote(owner), strconv.Quote(name)))
	return fmt.Errorf("%w: plugin %s would shadow global tool %s", ErrRegistrationRejected, strconv.Quote(owner), strconv.Quote(name))
}

// Decides a cross toolset replacement, logging the refusal, the denial or the audited override
func crossToolsetVerdict(d Def, owner, existingToolset string, denied bool) error {
	if d.Override {
		if denied {
			pkgLog().Error(fmt.Sprintf("Tool registration REJECTED: plugin %s attempted to override built-in tool %s (existing toolset %s) without operator opt-in. "+
				"Set plugins.entries.<plugin_id>.allow_tool_override: true in config.yaml to allow it.",
				strconv.Quote(owner), strconv.Quote(d.Name), strconv.Quote(existingToolset)))
			return &overrideDeniedError{owner: owner, name: d.Name}
		}
		pkgLog().Info(fmt.Sprintf("Tool %q: toolset %q overriding existing toolset %q (override=True opt-in)", d.Name, d.Toolset, existingToolset))
		return nil
	}
	pkgLog().Error(fmt.Sprintf("Tool registration REJECTED: %q (toolset %q) would shadow existing tool from toolset %q. "+
		"Pass override=True to register() if the replacement is intentional, or deregister the existing tool first.", d.Name, d.Toolset, existingToolset))
	return fmt.Errorf("%w: %s (toolset %s) would shadow existing tool from toolset %s", ErrRegistrationRejected,
		strconv.Quote(d.Name), strconv.Quote(d.Toolset), strconv.Quote(existingToolset))
}

// Logs and returns the refusal of a plugin removing a tool it neither owns nor may override
func rejectForeignDeregister(caller, name, toolset string) error {
	pkgLog().Error(fmt.Sprintf("Tool deregistration REJECTED: plugin %s attempted to remove tool %s (toolset %s) it does not own, without operator opt-in. "+
		"Set plugins.entries.<plugin_id>.allow_tool_override: true in config.yaml to allow it.",
		strconv.Quote(caller), strconv.Quote(name), strconv.Quote(toolset)))
	return &deregisterDeniedError{msg: fmt.Sprintf("Plugin module %s cannot deregister tool %s (toolset %s) without operator opt-in (allow_tool_override).",
		strconv.Quote(caller), strconv.Quote(name), strconv.Quote(toolset))}
}

// Resolves the scope a plugin caller acts in, refusing a scope outside the profiles it is bound to
func (e *registryExt) callerScopeLocked(callerOwner, scope string) (string, error) {
	scopes := e.moduleScopes[callerOwner]
	if len(scopes) == 0 {
		return scope, nil
	}
	if scope != "" {
		if !scopes[scope] {
			return "", &deregisterDeniedError{msg: fmt.Sprintf("Plugin module %s cannot deregister tools outside its own profile scope.", strconv.Quote(callerOwner))}
		}
		return scope, nil
	}
	if len(scopes) == 1 {
		for s := range scopes {
			return s, nil
		}
	}
	return "", &deregisterDeniedError{msg: fmt.Sprintf("Plugin module %s is active in multiple profiles and cannot register outside one of those scopes.", strconv.Quote(callerOwner))}
}
