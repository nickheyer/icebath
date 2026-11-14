//go:build linux

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Fails unless the path exists as a directory, or as a regular file when file is true
func requirePath(t *testing.T, path string, file bool) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || info.IsDir() == file {
		t.Fatalf("%s = %v, %v", path, info, err)
	}
}

// Fails unless nothing exists at the path, naming every entry found under it
func requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		var found []string
		_ = filepath.WalkDir(path, func(entry string, _ os.DirEntry, _ error) error {
			found = append(found, entry)
			return nil
		})
		t.Fatalf("%s is still there: %v %v", path, err, found)
	}
}

func TestRegistryGrantRenamesAndDeletesOnlyItsOwnServedProfile(t *testing.T) {
	model := map[string]any{"model": map[string]any{"provider": "custom", "default": "m", "base_url": "http://127.0.0.1:1/v1"},
		"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false}}}
	rootConfig := map[string]any{"gateway": map[string]any{"multiplex_profiles": true}}
	for k, v := range model {
		rootConfig[k] = v
	}
	tmp, h, user := gatewayCaseHomes(t, rootConfig)
	profilesDir := filepath.Join(h, "profiles")
	for _, name := range []string{"beta", "gamma", "keep"} {
		if err := os.MkdirAll(filepath.Join(profilesDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		writeJSONConfig(t, filepath.Join(profilesDir, name, "config.yaml"), model)
	}
	daemon := startGatewayChild(t, filepath.Join(tmp, "gateway.log"), gatewayChildEnv(h, user), gateway.GatewayRunModuleEntry)
	descriptor := map[string]any{}
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		select {
		case err := <-daemon.exited:
			daemon.stopped = true
			t.Fatalf("the gateway exited before it was ready: %v\n%s", err, daemon.output())
		default:
		}
		if identified, err := gatewayControl(h, "identify", nil); err == nil {
			descriptor = identified
			if served, _ := descriptor["served_profiles"].([]any); descriptor["state"] == "ready" && len(served) == 4 {
				break
			}
		}
	}
	if descriptor["state"] != "ready" {
		t.Fatalf("descriptor = %v\n%s", descriptor, daemon.output())
	}
	homeOf := func(profile string) string {
		dir := h
		if profile != "default" {
			dir = filepath.Join(profilesDir, profile)
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	grant := func(profile, purpose string) http.Header {
		identified, err := gatewayControl(h, "identify", nil)
		if err != nil {
			t.Fatal(err)
		}
		minted, err := gatewayControl(h, "session-ticket", map[string]any{"profile_id": homeOf(profile), "purpose": purpose,
			"instance_id": identified["instance_id"]})
		if err != nil {
			t.Fatal(err)
		}
		value, _ := minted["ticket"].(string)
		return http.Header{nativeTicketHeader: {value}}
	}
	ready := func() map[string]any {
		var settled map[string]any
		for range 200 {
			identified, err := gatewayControl(h, "identify", nil)
			if err != nil {
				t.Fatal(err)
			}
			settled = identified
			if settled["state"] == "ready" {
				return settled
			}
			time.Sleep(50 * time.Millisecond)
		}
		return settled
	}
	origin, _ := descriptor["api_origin"].(string)
	client := newOriginClient(t, origin)
	client.client.Timeout = 60 * time.Second
	send := func(method, path string, body any, header http.Header) (int, map[string]any, string) {
		text := ""
		if body != nil {
			raw, _ := json.Marshal(body)
			text = string(raw)
			header = header.Clone()
			header.Set("Content-Type", "application/json")
		}
		status, reply := client.call(method, path, header, text)
		var decoded map[string]any
		_ = json.Unmarshal([]byte(reply), &decoded)
		return status, decoded, reply
	}
	for _, request := range []struct {
		method string
		body   any
	}{{"DELETE", nil}, {"PATCH", map[string]any{"new_name": "renamed"}}} {
		status, _, text := send(request.method, "/api/profiles/beta", request.body, grant("beta", "native-http"))
		wantStatus(t, request.method+" beta with a native-http grant "+text, status, 403)
	}
	requirePath(t, filepath.Join(profilesDir, "beta"), false)
	registry := "native-profile-registry"
	status, _, text := send("DELETE", "/api/profiles/gamma", nil, grant("beta", registry))
	wantStatus(t, "DELETE gamma with beta's registry grant "+text, status, 403)
	requirePath(t, filepath.Join(profilesDir, "gamma"), false)
	for _, request := range []struct {
		method, path string
		body         any
	}{{"GET", "/api/config", nil}, {"GET", "/api/profiles/beta/soul", nil}, {"PUT", "/api/profiles/beta/soul", map[string]any{"content": "x"}},
		{"POST", "/api/profiles", map[string]any{"name": "sneaky"}}, {"POST", "/api/profiles/import", map[string]any{"archive": "x"}}} {
		status, _, text := send(request.method, request.path, request.body, grant("beta", registry))
		wantStatus(t, request.method+" "+request.path+" with a registry grant "+text, status, 401)
	}
	requireAbsent(t, filepath.Join(profilesDir, "sneaky"))
	status, reply, text := send("PATCH", "/api/profiles/gamma", map[string]any{"new_name": "delta"}, grant("gamma", registry))
	wantStatus(t, "rename gamma "+text, status, 200)
	if reply["name"] != "delta" {
		t.Fatalf("rename = %s", text)
	}
	requireAbsent(t, filepath.Join(profilesDir, "gamma"))
	requirePath(t, filepath.Join(profilesDir, "delta", "config.yaml"), true)
	ready()
	status, _, text = send("DELETE", "/api/profiles/beta", nil, grant("beta", registry))
	wantStatus(t, "delete beta "+text, status, 200)
	requireAbsent(t, filepath.Join(profilesDir, "beta"))
	ready()
	status, _, text = send("DELETE", "/api/profiles/keep", nil, grant("keep", registry))
	wantStatus(t, "delete keep "+text, status, 200)
	served := ready()
	var servedHomes []any
	for _, raw := range served["served_profiles"].([]any) {
		if profile, isDict := raw.(map[string]any); isDict {
			servedHomes = append(servedHomes, profile["profile_id"])
		}
	}
	if !slices.Contains(servedHomes, any(homeOf("default"))) {
		t.Fatalf("served = %v", served)
	}
	once := grant("default", registry)
	status, reply, text = send("PATCH", "/api/profiles/default", map[string]any{"new_name": "Main"}, once)
	wantStatus(t, "display rename of default "+text, status, 200)
	if reply["name"] != "default" || reply["display_name"] != "Main" {
		t.Fatalf("display rename = %s", text)
	}
	ready()
	status, _, _ = send("PATCH", "/api/profiles/default", map[string]any{"new_name": "Main"}, once)
	wantStatus(t, "reused registry grant", status, 401)
	status, _, text = send("PATCH", "/api/profiles/default", map[string]any{"new_name": "Main"}, grant("default", registry))
	wantStatus(t, "fresh registry grant "+text, status, 200)
	status, _, text = send("DELETE", "/api/profiles/default", nil, grant("default", registry))
	wantStatus(t, "delete default "+text, status, 403)
	requirePath(t, filepath.Join(h, "config.yaml"), true)
	ready()
	status, _, text = send("GET", "/api/profiles/delta/soul", nil, grant("delta", "native-http"))
	wantStatus(t, "delta soul "+text, status, 200)
}
