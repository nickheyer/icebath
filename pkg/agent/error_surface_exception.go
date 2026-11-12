package agent

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Python exception modules whose errors the API classifier owns
var surfaceAPIExcModulePrefixes = map[string]bool{"openai": true, "httpx": true, "httpcore": true, "anthropic": true,
	"botocore": true, "boto3": true, "google": true, "grpc": true, "requests": true, "aiohttp": true, "ssl": true, "socket": true, "urllib": true}

// Error declaring the Python exception class it stands for
type pyTypeNamed interface{ PyTypeName() string }

// Error declaring the Python module its exception class lives in
type pyModuled interface{ PyModule() string }

// Error carrying an HTTP status the way an SDK exception exposes status_code
type statusCoded interface{ StatusCode() int }

// Python class name of err, untyped Go errors standing for a bare Exception
func surfaceExceptionName(err error) string {
	if n, ok := err.(pyTypeNamed); ok && n.PyTypeName() != "" {
		return n.PyTypeName()
	}
	name := fmt.Sprintf("%T", err)
	switch name {
	case "*errors.errorString", "*fmt.wrapError", "*fmt.wrapErrors", "*errors.joinError":
		return "Exception"
	}
	name = strings.TrimLeft(name, "*")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// Top level Python module of err, the SDK, transport, TLS and socket errors mapped to their Python homes
func surfaceExceptionModule(err error) string {
	var moduled pyModuled
	if errors.As(err, &moduled) {
		return strings.SplitN(moduled.PyModule(), ".", 2)[0]
	}
	if errors.Is(err, provider.ErrNoAWSCredentials) {
		return "botocore"
	}
	var apiErr *provider.APIError
	var urlErr *url.Error
	var certErr *tls.CertificateVerificationError
	var recErr tls.RecordHeaderError
	var alertErr tls.AlertError
	var netErr net.Error
	switch {
	case errors.As(err, &apiErr):
		return "openai"
	case errors.As(err, &certErr), errors.As(err, &recErr), errors.As(err, &alertErr):
		return "ssl"
	case errors.As(err, &urlErr):
		return "httpx"
	case errors.As(err, &netErr):
		return "socket"
	}
	return "builtins"
}

// Status code of err when it carries one, the second result false when it has no status_code attribute
func surfaceExceptionStatus(err error) (int, bool) {
	var apiErr *provider.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode, true
	}
	var sc statusCoded
	if errors.As(err, &sc) {
		return sc.StatusCode(), true
	}
	return 0, false
}

// Descriptor for an error that escaped the turn dispatcher, API failures through the classifier and the rest gateway layer
func buildErrorSurfaceFromException(err error, providerName, model string, apiKey any) (surface map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			errorSurfaceLog().Debug("error_surface: exception classification failed", "error", fmt.Sprint(r))
			surface = nil
		}
	}()
	if err == nil {
		return nil
	}
	name := surfaceExceptionName(err)
	message := err.Error()
	if message == "" {
		message = name
	}
	if state.IsDiskFullError(err) {
		return errorSurface(LayerDisk, "disk_full", false, providerName, model)
	}
	status, hasStatus := surfaceExceptionStatus(err)
	if !surfaceAPIExcModulePrefixes[surfaceExceptionModule(err)] && !hasStatus {
		return errorSurface(LayerGateway, name, true, providerName, model)
	}
	key, _ := apiKey.(string)
	classified := provider.ClassifyError(provider.ClassifyInput{Err: err, Status: status, Provider: providerName, Model: model, APIKey: key})
	errText := classified.Message
	if errText == "" {
		errText = message
	}
	synthetic := map[string]any{"error": errText, "failure_reason": string(classified.Reason)}
	if resetsAt, ok := provider.ParseAbsoluteTimestamp(provider.ExtractAPIErrorContext(err)["reset_at"]); ok {
		synthetic["failure_resets_at"] = resetsAt
	}
	surface = buildErrorSurfaceFromResult(synthetic, providerName, model)
	if surface != nil {
		surface["retryable"] = classified.Retryable
	}
	return surface
}
