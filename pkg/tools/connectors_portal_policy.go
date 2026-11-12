package tools

// InvalidMemberPolicy is the ValueError of a member policy change the Portal would refuse
type InvalidMemberPolicy struct{ Msg string }

func (e *InvalidMemberPolicy) Error() string { return e.Msg }

// Whether a member layer mode lists allowed connectors, and the connectors it lists
func policyLayerValues(body PolicyBody) (bool, []string) {
	switch body.Mode {
	case "deny-all":
		return true, []string{}
	case "allow":
		return true, append([]string{}, body.Connectors...)
	case "deny":
		return false, append([]string{}, body.DisabledConnectors...)
	}
	return false, []string{}
}

// Write key, listed connectors and allow flag of the member layer, unrestricted when there is none
func policyLayer(member *PolicyBody) (string, []string, bool) {
	body := PolicyBody{Mode: "unrestricted"}
	if member != nil {
		body = *member
	}
	allow, values := policyLayerValues(body)
	if allow {
		return "connectors", values, true
	}
	return "disabledConnectors", values, false
}

// Connector slug of a change, refused as an invalid member policy when it is no slug
func policySlug(connector string) (string, error) {
	if err := ValidateSlug(connector); err != nil {
		return "", &InvalidMemberPolicy{Msg: "connector must be a slug"}
	}
	return connector, nil
}

// Disabled tool slugs of a change, refused when one is empty or longer than 256 characters
func policyValidatedTools(tools []string) ([]string, error) {
	for _, tool := range tools {
		if tool == "" || len([]rune(tool)) > 256 {
			return nil, &InvalidMemberPolicy{Msg: "disabled_tools must contain non-empty tool slugs up to 256 characters"}
		}
	}
	return tools, nil
}

func policyAnyList(items []string) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

// Member policy write turning one connector on or off within the member layer
func ComposeConnectorWrite(member *PolicyBody, connector string, enabled bool) (map[string]any, error) {
	connector, err := policySlug(connector)
	if err != nil {
		return nil, err
	}
	key, values, allow := policyLayer(member)
	retained := []string{}
	for _, item := range values {
		if item != connector {
			retained = append(retained, item)
		}
	}
	if enabled == allow {
		retained = append(retained, connector)
	}
	return map[string]any{"scope": "member", key: policyAnyList(retained)}, nil
}

// Member policy write replacing one enabled connector's disabled tools
func ComposeToolsWrite(member *PolicyBody, connector string, disabledTools []string) (map[string]any, error) {
	connector, err := policySlug(connector)
	if err != nil {
		return nil, err
	}
	key, values, allow := policyLayer(member)
	listed := false
	for _, item := range values {
		if item == connector {
			listed = true
		}
	}
	if listed != allow {
		return nil, &InvalidMemberPolicy{Msg: "connector is off in the member policy"}
	}
	tools, err := policyValidatedTools(disabledTools)
	if err != nil {
		return nil, err
	}
	return map[string]any{"scope": "member", key: policyAnyList(values), "tools": map[string]any{connector: policyAnyList(tools)}}, nil
}
