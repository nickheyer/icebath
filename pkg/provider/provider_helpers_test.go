package provider

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// Builds a JSON response for the request with the status, payload bytes or encoding, and extra headers
func jsonResponse(req *http.Request, status int, payload any, headers map[string]string) *http.Response {
	body, ok := payload.([]byte)
	if !ok {
		body, _ = json.Marshal(payload)
	}
	resp := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: req}
	resp.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		resp.Header.Set(k, v)
	}
	return resp
}

// Decodes a JSON text string, or the JSON encoding of any other value, into a map and panics on bad input
func jsonMap(v any) map[string]any {
	raw, ok := v.(string)
	if !ok {
		encoded, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		raw = string(encoded)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		panic(err)
	}
	return out
}

var (
	testSettleAfterUpgradeCalls int
	testBillingInvalidateCalls  int
	testSaveModelChoiceCalls    []string
	testGuestHandoffDrainResult []map[string]any
)

// Guest handoff replacement notes recorded by installNousRecorders
var testGuestHandoffNotes [][2]map[string]any

// Replaces the Nous login sibling steps with recorders and restores every Nous seam at cleanup
func installNousRecorders(t *testing.T) {
	t.Helper()
	prompt, save, drain, settle, invalidate, entitlement := nousPromptModelSelectionFn, nousSaveModelChoiceFn, nousGuestHandoffDrainFn, nousSettleAfterUpgradeFn, nousBillingInvalidateFn, nousEntitlementMessageFn
	testBillingInvalidateCalls, testSettleAfterUpgradeCalls, testSaveModelChoiceCalls, testGuestHandoffDrainResult = 0, 0, nil, nil
	nousSaveModelChoiceFn = func(env PoolEnv, modelID string) error {
		testSaveModelChoiceCalls = append(testSaveModelChoiceCalls, modelID)
		return nil
	}
	nousGuestHandoffDrainFn = func(env PoolEnv) ([]map[string]any, error) { return testGuestHandoffDrainResult, nil }
	note := nousGuestHandoffNoteFn
	testGuestHandoffNotes = nil
	nousGuestHandoffNoteFn = func(env PoolEnv, previous, next map[string]any) error {
		testGuestHandoffNotes = append(testGuestHandoffNotes, [2]map[string]any{previous, next})
		return nil
	}
	t.Cleanup(func() { nousGuestHandoffNoteFn = note })
	nousSettleAfterUpgradeFn = func(env PoolEnv, state map[string]any) { testSettleAfterUpgradeCalls++ }
	nousBillingInvalidateFn = func() { testBillingInvalidateCalls++ }
	scriptPickerAccounts(t, nil, nil)
	t.Cleanup(func() {
		nousPromptModelSelectionFn, nousSaveModelChoiceFn, nousGuestHandoffDrainFn, nousSettleAfterUpgradeFn, nousBillingInvalidateFn, nousEntitlementMessageFn = prompt, save, drain, settle, invalidate, entitlement
	})
}

// Scripts the post login picker with a function of the prompt alone
func scriptNousPicker(pick func(p ModelSelectionPrompt) (string, bool)) {
	nousPromptModelSelectionFn = func(env PoolEnv, out io.Writer, p ModelSelectionPrompt) (string, bool) { return pick(p) }
}

// Scripts the account entitlement lookup with a fixed message
func scriptNousEntitlement(message string) {
	nousEntitlementMessageFn = func(env PoolEnv, capability string) (string, error) { return message, nil }
}
