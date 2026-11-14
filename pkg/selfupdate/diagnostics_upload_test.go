package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
)

type duRequest struct {
	method, target string
	body           []byte
	headers        map[string]string
}

// Answers every urlopen with the status and body, recording each request
func duPinURLOpen(t *testing.T, status int, body []byte, err error) *[]duRequest {
	t.Helper()
	saved, savedBase, savedReq, savedPut := diagnosticsURLOpen, diagnosticsNASBase, diagnosticsRequestUploadURL, diagnosticsPutBundle
	t.Cleanup(func() {
		diagnosticsURLOpen, diagnosticsNASBase, diagnosticsRequestUploadURL, diagnosticsPutBundle = saved, savedBase, savedReq, savedPut
	})
	calls := &[]duRequest{}
	diagnosticsURLOpen = func(_ context.Context, method, target string, data []byte, headers map[string]string, _ time.Duration) ([]byte, int, error) {
		*calls = append(*calls, duRequest{method, target, data, headers})
		return body, status, err
	}
	return calls
}

func TestRequestUploadURLHappyPathPostsJSONAndReturnsDict(t *testing.T) {
	payload := `{"success": true, "id": "abc-123", "uploadUrl": "https://bucket.s3.amazonaws.com/uploads/abc-123.json.gz?sig", "viewUrl": "https://support.example.com/diagnostics/abc-123", "uploadExpiresInSeconds": 900}`
	calls := duPinURLOpen(t, 200, []byte(payload), nil)
	size := 512
	result, err := diagnosticsRequestUploadURL(context.Background(), "application/gzip", &size)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := pyjson.Loads(payload)
	if !pyjson.Equal(result, want) {
		t.Fatal(result)
	}
	req := (*calls)[0]
	var sent map[string]any
	json.Unmarshal(req.body, &sent)
	if req.method != "POST" || req.target != diagnosticsNASBase+"/api/diagnostics/upload-url" || sent["contentType"] != "application/gzip" ||
		sent["sizeBytes"] != float64(512) || req.headers["Content-Type"] != "application/json" {
		t.Fatal(req, sent)
	}
}

func TestRequestUploadURLNon2xxRaises(t *testing.T) {
	duPinURLOpen(t, 500, []byte("boom"), nil)
	_, err := diagnosticsRequestUploadURL(context.Background(), "application/gzip", nil)
	var pe *pm.PyError
	if !errors.As(err, &pe) || pe.Class != "RuntimeError" {
		t.Fatal(err)
	}
}

func TestRequestUploadURLBaseURLEnvOverride(t *testing.T) {
	calls := duPinURLOpen(t, 200, []byte(`{"uploadUrl": "u", "id": "i", "viewUrl": "v"}`), nil)
	t.Setenv("ICEBATH_DIAGNOSTICS_BASE_URL", "https://staging.example.com")
	diagnosticsNASBase = diagnosticsBaseURL()
	if diagnosticsNASBase != "https://staging.example.com" {
		t.Fatal(diagnosticsNASBase)
	}
	if _, err := diagnosticsRequestUploadURL(context.Background(), "application/gzip", nil); err != nil {
		t.Fatal(err)
	}
	if (*calls)[0].target != "https://staging.example.com/api/diagnostics/upload-url" {
		t.Fatal((*calls)[0].target)
	}
}

func TestPutBundlePutSendsExactBodyAndContentType(t *testing.T) {
	calls := duPinURLOpen(t, 200, nil, nil)
	data := []byte("\x1f\x8b\x08gzipped-bytes")
	if err := diagnosticsPutBundle(context.Background(), "https://bucket.s3.amazonaws.com/uploads/x.json.gz?sig", data, "application/gzip"); err != nil {
		t.Fatal(err)
	}
	req := (*calls)[0]
	if req.method != "PUT" || string(req.body) != string(data) || req.headers["Content-Type"] != "application/gzip" {
		t.Fatal(req)
	}
}

func TestPutBundleHTTPErrorPropagates(t *testing.T) {
	duPinURLOpen(t, 500, nil, &pm.PyError{Class: "HTTPError", Message: "HTTP Error 500: err"})
	err := diagnosticsPutBundle(context.Background(), "https://u", []byte("data"), "application/gzip")
	var pe *pm.PyError
	if !errors.As(err, &pe) || pe.Class != "HTTPError" {
		t.Fatal(err)
	}
}

func TestShareToNousOrchestratesRequestThenPut(t *testing.T) {
	duPinURLOpen(t, 200, nil, nil)
	info := pyjson.ObjectOf("id", "id-9", "uploadUrl", "https://bucket/uploads/id-9.json.gz?sig", "viewUrl", "https://support/diagnostics/id-9",
		"expiresAt", "2026-06-20T00:00:00Z")
	blob := []byte("gzipped-bundle")
	var gotSize int
	reqCalls := 0
	diagnosticsRequestUploadURL = func(_ context.Context, _ string, size *int) (*pyjson.Object, error) {
		reqCalls++
		gotSize = *size
		return info, nil
	}
	var put []any
	diagnosticsPutBundle = func(_ context.Context, u string, data []byte, ct string) error {
		put = append(put, u, string(data), ct)
		return nil
	}
	result, err := debugShareToNous(context.Background(), blob)
	if err != nil || result != info || reqCalls != 1 || gotSize != len(blob) || len(put) != 3 ||
		put[0] != info.Vals["uploadUrl"] || put[1] != string(blob) || put[2] != "application/gzip" {
		t.Fatal(result, err, put)
	}
}

func TestShareToNousPutFailurePropagates(t *testing.T) {
	duPinURLOpen(t, 200, nil, nil)
	diagnosticsRequestUploadURL = func(context.Context, string, *int) (*pyjson.Object, error) {
		return pyjson.ObjectOf("id", "id-9", "uploadUrl", "https://u", "viewUrl", "v"), nil
	}
	diagnosticsPutBundle = func(context.Context, string, []byte, string) error {
		return &pm.PyError{Class: "RuntimeError", Message: "PUT failed"}
	}
	if _, err := debugShareToNous(context.Background(), []byte("data")); err == nil {
		t.Fatal("no error")
	}
}

func TestShareToNousShareSucceedsWithoutIDInResponse(t *testing.T) {
	duPinURLOpen(t, 200, nil, nil)
	info := pyjson.ObjectOf("uploadUrl", "https://u", "viewUrl", "v")
	diagnosticsRequestUploadURL = func(context.Context, string, *int) (*pyjson.Object, error) { return info, nil }
	puts := 0
	diagnosticsPutBundle = func(context.Context, string, []byte, string) error { puts++; return nil }
	if result, err := debugShareToNous(context.Background(), []byte("data")); err != nil || result != info || puts != 1 {
		t.Fatal(result, err)
	}
}
