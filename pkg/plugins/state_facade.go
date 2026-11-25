package plugins

import (
	"context"
	"fmt"
)

// Python attribute of the plugin state store a hosted plugin reads, get and set answering as methods
func (s *PluginState) HostAttr(_ context.Context, name string) (any, bool, bool, error) {
	switch name {
	case "data_dir":
		return s.DataDir(), false, false, nil
	case "path":
		return s.Path(), false, false, nil
	case "quota_bytes":
		return int64(s.QuotaBytes()), false, false, nil
	case "get", "set":
		return nil, true, false, nil
	}
	return nil, false, false, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("plugin state has no %s", name)}
}

// Runs get(key, default=None) or set(key, value) of the plugin state store for a hosted plugin
func (s *PluginState) HostCall(_ context.Context, method string, args []any, kwargs map[string]any) (any, error) {
	switch method {
	case "get":
		a, _, err := bindPyOn("PluginState", method, []paramValue{req("key"), opt("default", nil)}, args, kwargs)
		if err != nil {
			return nil, err
		}
		return s.Get(argStr(a["key"]), a["default"])
	case "set":
		a, _, err := bindPyOn("PluginState", method, []paramValue{req("key"), req("value")}, args, kwargs)
		if err != nil {
			return nil, err
		}
		return nil, s.Set(argStr(a["key"]), a["value"])
	}
	return nil, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("plugin state has no %s", method)}
}
