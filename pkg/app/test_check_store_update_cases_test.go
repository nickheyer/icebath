package app

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	storeCaseDownloading storeUpdateState = 1
	storeCaseCanceled    storeUpdateState = 4
)

type storeCaseOperation struct {
	updates storeUpdateList
	outcome storeUpdateOutcome
}

func (storeCaseOperation) Status() (storeAsyncStatus, error) { return 1, nil }
func (storeCaseOperation) Cancel() error                     { return nil }

type storeCaseUpdatesOperation struct{ storeCaseOperation }

func (o storeCaseUpdatesOperation) GetResults() (storeUpdateList, error) { return o.updates, nil }

type storeCaseOutcomeOperation struct{ storeCaseOperation }

func (o storeCaseOutcomeOperation) GetResults() (storeUpdateOutcome, error) { return o.outcome, nil }

type storeCaseUpdates []string

func (u storeCaseUpdates) Len() (int, error)              { return len(u), nil }
func (u storeCaseUpdates) FullName(i int) (string, error) { return u[i], nil }

type storeCaseOutcome storeUpdateState

func (o storeCaseOutcome) OverallState() (storeUpdateState, error) { return storeUpdateState(o), nil }

type storeCaseContext struct {
	t       *testing.T
	updates storeCaseUpdates
	state   storeUpdateState
	calls   []string
}

func (c *storeCaseContext) GetAppAndOptionalStorePackageUpdatesAsync() (storeUpdatesOperation, error) {
	c.calls = append(c.calls, "check")
	return storeCaseUpdatesOperation{storeCaseOperation{updates: c.updates}}, nil
}

func (c *storeCaseContext) request(kind string, updates storeUpdateList) (storeOutcomeOperation, error) {
	if got, ok := updates.(storeCaseUpdates); !ok || len(got) != len(c.updates) || (len(got) > 0 && &got[0] != &c.updates[0]) {
		c.t.Fatalf("%s got another update sequence %v", kind, updates)
	}
	c.calls = append(c.calls, kind)
	return storeCaseOutcomeOperation{storeCaseOperation{outcome: storeCaseOutcome(c.state)}}, nil
}

func (c *storeCaseContext) RequestDownloadStorePackageUpdatesAsync(updates storeUpdateList) (storeOutcomeOperation, error) {
	return c.request("download", updates)
}

func (c *storeCaseContext) RequestDownloadAndInstallStorePackageUpdatesAsync(updates storeUpdateList) (storeOutcomeOperation, error) {
	return c.request("install", updates)
}

func storeCaseWait(storeOperation) error { return nil }

func TestStoreOperationPassesUpdateSequenceAndRequiresCompleted(t *testing.T) {
	for _, mode := range []string{"check", "download", "install"} {
		for _, state := range []storeUpdateState{storeUpdateCompleted, storeCaseCanceled, storeCaseDownloading} {
			t.Run(mode+"-"+state.name(), func(t *testing.T) {
				context := &storeCaseContext{t: t, updates: storeCaseUpdates{"package-v2"}, state: state}
				result, err := storePerform(context, mode, storeCaseWait, storeUpdateCompleted)
				if err != nil {
					t.Fatal(err)
				}
				want := []string{"check"}
				if mode != "check" {
					want = append(want, mode)
				}
				if !slices.Equal(context.calls, want) {
					t.Fatalf("calls %v want %v", context.calls, want)
				}
				if available, _ := result.Get("available"); available != true {
					t.Fatalf("available %v", available)
				}
				if packages, _ := result.Get("packages"); !pyrepr.Equal(packages, []any{"package-v2"}) {
					t.Fatalf("packages %v", packages)
				}
				if mode != "check" {
					if ok, _ := result.Get("ok"); ok != (state == storeUpdateCompleted) {
						t.Fatalf("ok %v for state %s", ok, state.name())
					}
					if _, has := result.Get("error"); has != (state != storeUpdateCompleted) {
						t.Fatalf("error present %v for state %s", has, state.name())
					}
				}
				empty := &storeCaseContext{t: t, state: storeUpdateCompleted}
				got, err := storePerform(empty, mode, storeCaseWait, storeUpdateCompleted)
				if err != nil {
					t.Fatal(err)
				}
				if !pyjson.Equal(got, pyjson.ObjectOf("available", false, "packages", []any{}, "ok", true)) {
					t.Fatalf("empty result %v", got)
				}
				if !slices.Equal(empty.calls, []string{"check"}) {
					t.Fatalf("empty calls %v", empty.calls)
				}
			})
		}
	}
}

func TestEntryReportsJSONWhenTheProjectionIsUnavailable(t *testing.T) {
	saved := storeInitApartment
	storeInitApartment = func() error { return errors.New("fixture unavailable") }
	t.Cleanup(func() { storeInitApartment = saved })
	var stdout, stderr bytes.Buffer
	if code := WindowsStoreUpdateMain([]string{"--mode", "check"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	payload, err := pyjson.Loads(stdout.String())
	if err != nil {
		t.Fatal(err)
	}
	row, ok := pyjson.AsObject(payload)
	if !ok {
		t.Fatalf("payload %v", payload)
	}
	okValue, _ := row.Get("ok")
	available, hasAvailable := row.Get("available")
	if okValue != false || !hasAvailable || available != nil {
		t.Fatalf("payload %s", stdout.String())
	}
	if text, _ := row.Get("error"); !strings.Contains(pyrepr.Str(text), "fixture unavailable") {
		t.Fatalf("error %v", text)
	}
}
