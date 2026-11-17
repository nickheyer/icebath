package provider

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestWelcomeTrialTheTokenSeamRefusesAnInferenceTokenToAGuestWithoutTheTrial(t *testing.T) {
	portal, env := installAnonPortal(t)
	held, err := EnsurePortalIdentity(env, PortalIdentityOptions{Explicit: true})
	if err != nil || held == nil {
		t.Fatalf("%v %v", held, err)
	}
	guest := map[string]any{}
	for k, v := range held {
		guest[k] = v
	}
	_, err = refreshGuestState(env, guest, &http.Client{Timeout: 15 * time.Second}, "inference")
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("%T %v", err, err)
	}
	for _, purpose := range anonPurposes(portal) {
		if purpose != "connectors" {
			t.Fatal(anonPurposes(portal))
		}
	}
}
