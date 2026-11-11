package skills

import "context"

// Process wide secret capture callback resolving its owner from the calling context
type ContextSecretCaptureFunc func(ctx context.Context, name, prompt string, metadata map[string]any) (map[string]any, error)

var secretCaptureContextCallback ContextSecretCaptureFunc

// Installs the process wide secret capture callback that reads the caller's session from its context
func SetContextSecretCaptureCallback(cb ContextSecretCaptureFunc) {
	secretCaptureMu.Lock()
	secretCaptureContextCallback, secretCaptureCallback = cb, nil
	secretCaptureMu.Unlock()
}

// Process wide capture callback bound to the calling context, nil when no surface installed one
func processSecretCapture(ctx context.Context) SecretCaptureFunc {
	secretCaptureMu.RLock()
	defer secretCaptureMu.RUnlock()
	if contextual := secretCaptureContextCallback; contextual != nil {
		return func(name, prompt string, metadata map[string]any) (map[string]any, error) {
			return contextual(ctx, name, prompt, metadata)
		}
	}
	return secretCaptureCallback
}
