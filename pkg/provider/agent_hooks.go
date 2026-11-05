package provider

// Agent outbound kwargs sweep the agent package installs at init, run on hand built summary kwargs
var SanitizeOutboundKwargsHook func(agent any, apiKwargs map[string]any)

// Agent per model image strip the agent package installs at init, true when an image part was removed
var StripImagesForRejectingModelHook func(agent any, apiMessages *[]any) bool
