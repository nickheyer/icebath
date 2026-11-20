package provider

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Ports probed for a llama-server beside the managed and configured ones, always on 127.0.0.1
var DefaultProbePorts = []int{8080}

// A running llama-server, BaseURL its OpenAI compatible v1 root and NCtx nil when unreported
type DetectedServer struct {
	BaseURL      string
	BuildInfo    string
	ModelPath    string
	NCtx         any
	RouterMode   bool
	AuthRequired bool
}

var detectHTTPClient = &http.Client{}

// Status and decoded JSON body of a GET, status 0 on transport or JSON failure, err when the body cannot be decoded as text or arrives cut
func detectGet(url string, timeout time.Duration) (int, any, error) {
	client := *detectHTTPClient
	client.Timeout = timeout
	resp, err := client.Get(url)
	if err != nil {
		return 0, nil, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, nil, nil
	}
	raw, err := io.ReadAll(resp.Body)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, nil, fmt.Errorf("IncompleteRead(%d bytes read)", len(raw))
	}
	if err != nil {
		return 0, nil, nil
	}
	if len(raw) == 0 {
		return resp.StatusCode, nil, nil
	}
	text, err := hfJSONText(raw)
	if err != nil {
		return 0, nil, err
	}
	body, ok := moaJSONLoads(text)
	if !ok {
		return 0, nil, nil
	}
	return resp.StatusCode, body, nil
}

// One port, the props fingerprint first, then the models listing for router capability
func ProbePort(port int) (*DetectedServer, error) {
	root := fmt.Sprintf("http://127.0.0.1:%d", port)
	status, props, err := detectGet(root+"/props", 3*time.Second)
	if err != nil {
		return nil, err
	}
	if status == 401 {
		return &DetectedServer{BaseURL: root + "/v1", AuthRequired: true}, nil
	}
	fields, isDict := props.(map[string]any)
	if status != 200 || !isDict {
		return nil, nil
	}
	build := pyStr(pyGetAny(fields, "build_info", ""))
	if build == "" {
		return nil, nil
	}
	var nCtx any
	if dgs, ok := fields["default_generation_settings"].(map[string]any); ok {
		nCtx = dgs["n_ctx"]
	}
	modelsStatus, models, err := detectGet(root+"/models", 3*time.Second)
	if err != nil {
		return nil, err
	}
	listing, listingIsDict := models.(map[string]any)
	_, hasData := listing["data"]
	return &DetectedServer{BaseURL: root + "/v1", BuildInfo: build, ModelPath: pyStr(pyGetAny(fields, "model_path", "")),
		NCtx: nCtx, RouterMode: modelsStatus == 200 && listingIsDict && hasData}, nil
}

// First server found across the default ports then extraPorts, each port probed once
func DetectServer(extraPorts []int) (*DetectedServer, error) {
	seen := map[int]bool{}
	for _, port := range append(append([]int{}, DefaultProbePorts...), extraPorts...) {
		if seen[port] {
			continue
		}
		seen[port] = true
		hit, err := ProbePort(port)
		if err != nil || hit != nil {
			return hit, err
		}
	}
	return nil, nil
}
