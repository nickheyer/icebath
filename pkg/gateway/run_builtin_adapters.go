package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Adapter class a platform module exports, its constructor and the class attributes it declares
type AdapterClass struct {
	Name   string
	New    func(config *PlatformConfig) (PlatformAdapter, error)
	Traits AdapterTraits
}

// Module, adapter class, requirements probe and the warning logged when the probe fails
type BuiltinAdapterSpec struct {
	Module      string
	AdapterName string
	Requirement string
	Warning     string
}

// Core adapters by platform, every other platform ships as a plugin
var BuiltinAdapters = map[Platform]BuiltinAdapterSpec{
	PlatformAPIServer: {"api_server", "APIServerAdapter", "check_api_server_requirements",
		"API Server: aiohttp not installed"},
	PlatformWebhook: {"webhook", "WebhookAdapter", "check_webhook_requirements",
		"Webhook: aiohttp not installed"},
	PlatformMSGraphWebhook: {"msgraph_webhook", "MSGraphWebhookAdapter", "check_msgraph_webhook_requirements",
		"MSGraph webhook: aiohttp not installed"},
}

// Attributes of the gateway platform modules by module name, adapter classes and requirement probes
var builtinPlatformModules = map[string]map[string]any{
	"webhook": {
		"WebhookAdapter": &AdapterClass{Name: "WebhookAdapter", Traits: webhookAdapterTraits(),
			New: func(config *PlatformConfig) (PlatformAdapter, error) {
				adapter, err := NewWebhookAdapter(config)
				if err != nil {
					return nil, err
				}
				return adapter, nil
			}},
		"check_webhook_requirements": CheckWebhookRequirements,
	},
	"msgraph_webhook": {
		"MSGraphWebhookAdapter": &AdapterClass{Name: "MSGraphWebhookAdapter", Traits: msgraphWebhookAdapterTraits(),
			New: func(config *PlatformConfig) (PlatformAdapter, error) {
				adapter, err := NewMSGraphWebhookAdapter(config)
				if err != nil {
					return nil, err
				}
				return adapter, nil
			}},
		"check_msgraph_webhook_requirements": CheckMSGraphWebhookRequirements,
	},
}

// Adapter class and requirements probe of a gateway platform module, the import or attribute error when absent
func BuiltinAdapterImport(module, adapterName, requirement string) (*AdapterClass, any, error) {
	qualified := "gateway.platforms." + module
	attrs, ok := builtinPlatformModules[module]
	if !ok {
		return nil, nil, &PyError{Kind: "ModuleNotFoundError", Msg: fmt.Sprintf("No module named '%s'", qualified)}
	}
	for _, name := range []string{adapterName, requirement} {
		if _, has := attrs[name]; !has {
			return nil, nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("module '%s' has no attribute '%s'", qualified, name)}
		}
	}
	cls, _ := attrs[adapterName].(*AdapterClass)
	return cls, attrs[requirement], nil
}

// Result of a requirements probe, called when callable and else read for truth
func requirementsMet(probe any) bool {
	if check, ok := probe.(func() bool); ok {
		return check()
	}
	return pyrepr.Truthy(probe)
}

// Core adapter of a platform, nil when the platform is not builtin or its requirements are unmet
func InstantiateBuiltinAdapter(ctx context.Context, platform Platform, config *PlatformConfig) (PlatformAdapter, error) {
	spec, ok := BuiltinAdapters[platform]
	if !ok {
		return nil, nil
	}
	cls, probe, err := BuiltinAdapterImport(spec.Module, spec.AdapterName, spec.Requirement)
	if err != nil {
		return nil, err
	}
	if !requirementsMet(probe) {
		slog.Default().With("logger", "gateway.run").WarnContext(ctx, spec.Warning)
		return nil, nil
	}
	return cls.New(config)
}
