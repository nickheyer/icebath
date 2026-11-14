package cron

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type rpcClientFunc func(method string, params map[string]any) (map[string]any, error)

func (f rpcClientFunc) RPC(_ context.Context, method string, params map[string]any) (map[string]any, error) {
	return f(method, params)
}
func (f rpcClientFunc) Close() error { return nil }

// Gateway connector and owner lookup replaced for one test
func replaceCanonicalDoors(t *testing.T, connect func(context.Context) (cronRPCClient, error), owner func(string) (any, bool)) {
	t.Helper()
	savedConnect, savedOwner, savedOp := connectGateway, cronOwnerForHome, cronOwnerOperation
	t.Cleanup(func() { connectGateway, cronOwnerForHome, cronOwnerOperation = savedConnect, savedOwner, savedOp })
	connectGateway = connect
	if owner == nil {
		owner = func(string) (any, bool) { return nil, false }
	}
	cronOwnerForHome = owner
}

type gatewayClientError struct{ msg string }

func (e *gatewayClientError) Error() string      { return e.msg }
func (e *gatewayClientError) PyTypeName() string { return "GatewayClientError" }

func TestAgentCronIsTransportOnlyAndRefusesWithoutOwner(t *testing.T) {
	s, _ := newTestStore(t)
	var calls []string
	replaceCanonicalDoors(t, func(context.Context) (cronRPCClient, error) {
		calls = append(calls, "connect")
		return nil, &gatewayClientError{"owner_unavailable"}
	}, nil)
	out, err := s.runJob(context.Background(), &Job{ID: "job", Prompt: "test"}, runJobOptions{ExecutionID: strPtr("fire")})
	if err != nil || len(calls) != 1 || out.Success || out.Error == nil || !strings.Contains(*out.Error, "owner_unavailable") {
		t.Fatalf("out %+v err %v calls %v", out, err, calls)
	}
}

func TestOwnerTickerUsesDirectAdmissionWithoutLoopback(t *testing.T) {
	s, _ := newTestStore(t)
	authority := &struct{ name string }{"owner"}
	replaceCanonicalDoors(t, func(context.Context) (cronRPCClient, error) {
		t.Fatal("loopback connect")
		return nil, nil
	}, func(string) (any, bool) { return authority, true })
	var ops []string
	cronOwnerOperation = func(_ context.Context, owner any, op string, _ map[string]any) (map[string]any, error) {
		if owner != authority {
			t.Fatalf("owner %v", owner)
		}
		ops = append(ops, op)
		if op == "submit" {
			return map[string]any{"session_id": "cron-fire", "admission_id": "admit"}, nil
		}
		return map[string]any{"status": "terminal", "result": []any{true, "document", "answer", nil}}, nil
	}
	out, err := s.runCanonicalJob(context.Background(), &Job{ID: "job"}, nil, nil, strPtr("fire"))
	if err != nil || !out.Success || out.Output != "document" || out.FinalResponse != "answer" || out.Error != nil || strings.Join(ops, ",") != "submit,status" {
		t.Fatalf("out %+v err %v ops %v", out, err, ops)
	}
}

func TestLostCronAckRetainsIdentityInsteadOfReportingFailure(t *testing.T) {
	s, _ := newTestStore(t)
	replaceCanonicalDoors(t, func(context.Context) (cronRPCClient, error) {
		return rpcClientFunc(func(string, map[string]any) (map[string]any, error) {
			return nil, &gatewayClientError{"Gateway disconnected"}
		}), nil
	}, nil)
	_, err := s.runCanonicalJob(context.Background(), &Job{ID: "job"}, nil, nil, strPtr("fire"))
	var unknown *CronExecutionUnknown
	if !errors.As(err, &unknown) {
		t.Fatalf("err %v", err)
	}
	records, _ := filepath.Glob(filepath.Join(s.home, "cron", "admissions", "*.json"))
	if len(records) != 1 {
		t.Fatalf("records %v", records)
	}
	var record struct {
		Params map[string]any `json:"params"`
	}
	raw, _ := os.ReadFile(records[0])
	if json.Unmarshal(raw, &record) != nil || record.Params["request_id"] != "fire" {
		t.Fatalf("record %s", raw)
	}
}

func TestDirectRetryReusesGeneratedFireIdentity(t *testing.T) {
	for _, initial := range []struct {
		name string
		id   *string
	}{{"empty", nil}, {"execution_id_none", nil}} {
		t.Run(initial.name, func(t *testing.T) {
			s, _ := newTestStore(t)
			var requests []string
			replaceCanonicalDoors(t, func(context.Context) (cronRPCClient, error) {
				return rpcClientFunc(func(_ string, params map[string]any) (map[string]any, error) {
					requests = append(requests, params["request_id"].(string))
					return nil, &gatewayClientError{"disconnected"}
				}), nil
			}, nil)
			job := &Job{ID: "direct-fire", ExecutionID: initial.id}
			for range 2 {
				_, err := s.runCanonicalJob(context.Background(), job, nil, nil, nil)
				var unknown *CronExecutionUnknown
				if !errors.As(err, &unknown) {
					t.Fatalf("err = %v", err)
				}
			}
			if len(requests) != 2 || requests[0] != requests[1] {
				t.Fatalf("requests = %v", requests)
			}
		})
	}
}
