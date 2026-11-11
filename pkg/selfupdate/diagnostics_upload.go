package selfupdate

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// NAS base url, ICEBATH_DIAGNOSTICS_BASE_URL pointing it at staging or a local instance
func diagnosticsBaseURL() string {
	if v, ok := os.LookupEnv("ICEBATH_DIAGNOSTICS_BASE_URL"); ok {
		return v
	}
	return "https://portal.nousresearch.com"
}

var (
	diagnosticsNASBase = diagnosticsBaseURL()
	diagnosticsURLOpen = debugURLOpen
)

const (
	diagnosticsRequestTimeout = 30 * time.Second
	diagnosticsUploadTimeout  = 120 * time.Second
)

// Response body of a request, a RuntimeError on a non 2xx status
func diagnosticsURLOpenChecked(ctx context.Context, target string, data []byte, method string, headers map[string]string, timeout time.Duration, what string) ([]byte, error) {
	all := map[string]string{"User-Agent": debugUserAgent}
	for k, v := range headers {
		all[k] = v
	}
	body, status, err := diagnosticsURLOpen(ctx, method, target, data, all, timeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &pm.PyError{Class: "RuntimeError", Message: fmt.Sprintf("%s failed: HTTP %d", what, status)}
	}
	return body, nil
}

// Presigned PUT url NAS mints for a diagnostics bundle, carrying uploadUrl, viewUrl and id
var diagnosticsRequestUploadURL = func(ctx context.Context, contentType string, sizeBytes *int) (*pyjson.Object, error) {
	payload := pyjson.ObjectOf("contentType", contentType)
	if sizeBytes != nil {
		payload.Set("sizeBytes", *sizeBytes)
	}
	encoded, err := pyjson.Dumps(payload)
	if err != nil {
		return nil, err
	}
	raw, err := diagnosticsURLOpenChecked(ctx, diagnosticsNASBase+"/api/diagnostics/upload-url", []byte(encoded), http.MethodPost,
		map[string]string{"Content-Type": "application/json", "Accept": "application/json"}, diagnosticsRequestTimeout,
		"diagnostics upload-url request")
	if err != nil {
		return nil, err
	}
	body, err := pystr.DecodeUTF8(raw)
	if err != nil {
		return nil, &pm.PyError{Class: "UnicodeDecodeError", Message: err.Error()}
	}
	result, err := pyjson.Loads(body)
	if err != nil {
		return nil, &pm.PyError{Class: "RuntimeError", Message: "diagnostics upload-url returned non-JSON response: " + RedactDebugSupportTextCapped(body, 200)}
	}
	obj, ok := pyjson.AsObject(result)
	if !ok || !pyrepr.Truthy(obj.Vals["uploadUrl"]) {
		return nil, &pm.PyError{Class: "RuntimeError", Message: "diagnostics upload-url response missing 'uploadUrl': " + RedactDebugSupportTextCapped(body, 200)}
	}
	return obj, nil
}

// PUTs the gzipped bundle to a presigned upload url with the content type NAS pinned when signing
var diagnosticsPutBundle = func(ctx context.Context, uploadURL string, data []byte, contentType string) error {
	_, err := diagnosticsURLOpenChecked(ctx, uploadURL, data, http.MethodPut, map[string]string{"Content-Type": contentType},
		diagnosticsUploadTimeout, "diagnostics bundle PUT")
	return err
}

// Mints a presigned PUT url for the exact bundle size then uploads the bundle to it
var debugShareToNous = func(ctx context.Context, bundle []byte) (*pyjson.Object, error) {
	size := len(bundle)
	info, err := diagnosticsRequestUploadURL(ctx, "application/gzip", &size)
	if err != nil {
		return nil, err
	}
	uploadURL, ok := info.Vals["uploadUrl"].(string)
	if !ok {
		return nil, &pm.PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(info.Vals["uploadUrl"]))}
	}
	if err := diagnosticsPutBundle(ctx, uploadURL, bundle, "application/gzip"); err != nil {
		return nil, err
	}
	return info, nil
}
