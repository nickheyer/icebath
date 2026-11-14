package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const detectTimeout = 3 * time.Second

var defaultProbePorts = []int{8080}

type DetectedServer struct {
	BaseURL      string
	BuildInfo    string
	ModelPath    string
	NCtx         *int
	RouterMode   bool
	AuthRequired bool
}

func (r *Runtime) probeGet(ctx context.Context, rawURL string) (int, any) {
	reqCtx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return resp.StatusCode, nil
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil
	}
	if len(raw) == 0 {
		return resp.StatusCode, nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, nil
	}
	return resp.StatusCode, out
}

func (r *Runtime) probePort(ctx context.Context, port int) *DetectedServer {
	root := fmt.Sprintf("http://127.0.0.1:%d", port)
	status, props := r.probeGet(ctx, root+"/props")
	if status == http.StatusUnauthorized {
		return &DetectedServer{BaseURL: root + "/v1", AuthRequired: true}
	}
	fields, ok := props.(map[string]any)
	if status != http.StatusOK || !ok {
		return nil
	}
	build := jsonText(fields["build_info"])
	if build == "" {
		return nil
	}
	hit := &DetectedServer{BaseURL: root + "/v1", BuildInfo: build, ModelPath: jsonText(fields["model_path"])}
	if dgs, ok := fields["default_generation_settings"].(map[string]any); ok {
		if n, ok := dgs["n_ctx"].(float64); ok {
			v := int(n)
			hit.NCtx = &v
		}
	}
	modelsStatus, models := r.probeGet(ctx, root+"/models")
	if m, ok := models.(map[string]any); ok && modelsStatus == http.StatusOK {
		_, hit.RouterMode = m["data"]
	}
	return hit
}

func (r *Runtime) DetectServer(ctx context.Context, extraPorts []int) *DetectedServer {
	seen := map[int]bool{}
	for _, port := range append(append([]int{}, defaultProbePorts...), extraPorts...) {
		if seen[port] {
			continue
		}
		seen[port] = true
		if hit := r.probePort(ctx, port); hit != nil {
			return hit
		}
	}
	return nil
}
