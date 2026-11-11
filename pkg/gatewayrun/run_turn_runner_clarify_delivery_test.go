package gatewayrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Clarify registration for session sk with the clarify timeout set in a fresh home
func armedClarify(t *testing.T, clarifyID string, timeout int) *tools.Env {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	body := []byte("clarify:\n  timeout: " + strconv.Itoa(timeout) + "\n")
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	tools.RegisterGatewayClarify(clarifyID, "sk", "pick", []string{"A", "B"}, false)
	t.Cleanup(func() { tools.ClearClarifySession("sk") })
	return &tools.Env{Home: dir}
}

// Future already settled with the outcome
func settledFuture(result any, err error) *concurrentFuture {
	fut := newConcurrentFuture()
	fut.set(result, err)
	return fut
}

func TestTimeoutKeepsRegistrationArmedAndProceedsToWait(t *testing.T) {
	armedClarify(t, "cid-timeout", 600)
	if disposition := clarifySendDisposition(newConcurrentFuture(), "sk"); disposition != "" {
		t.Fatalf("a send timeout aborted the clarify wait: %q", disposition)
	}
	if !tools.HasPendingClarify("sk") {
		t.Fatal("registration torn down on an ambiguous send")
	}
}

func TestSuccessfulSendProceedsToWait(t *testing.T) {
	armedClarify(t, "cid-ok", 600)
	if disposition := clarifySendDisposition(settledFuture(gateway.SendResult{Success: true}, nil), "sk"); disposition != "" {
		t.Fatalf("disposition %q", disposition)
	}
	if !tools.HasPendingClarify("sk") {
		t.Fatal("registration torn down on a sent card")
	}
}

func TestDefinitiveErrorResultTearsDownAndAborts(t *testing.T) {
	armedClarify(t, "cid-err", 600)
	fut := settledFuture(gateway.SendResult{Success: false, Error: "relay prompt op unavailable"}, nil)
	if disposition := clarifySendDisposition(fut, "sk"); disposition != clarifyUndelivered {
		t.Fatalf("disposition %q", disposition)
	}
	if tools.HasPendingClarify("sk") {
		t.Fatal("registration kept after a definitive failure")
	}
}

func TestNonTimeoutExceptionTearsDownAndAborts(t *testing.T) {
	armedClarify(t, "cid-exc", 600)
	if disposition := clarifySendDisposition(settledFuture(nil, errors.New("loop unavailable")), "sk"); disposition != clarifyUndelivered {
		t.Fatalf("disposition %q", disposition)
	}
	if tools.HasPendingClarify("sk") {
		t.Fatal("registration kept after a send exception")
	}
}

func TestMissingFutureTearsDownAndAborts(t *testing.T) {
	armedClarify(t, "cid-none", 600)
	if disposition := clarifySendDisposition(nil, "sk"); disposition != clarifyUndelivered {
		t.Fatalf("disposition %q", disposition)
	}
	if tools.HasPendingClarify("sk") {
		t.Fatal("registration kept without a future")
	}
}

// Resolves the clarify id with the response once its send has been classified
func answerLater(clarifyID, response string, after time.Duration) {
	go func() {
		time.Sleep(after)
		tools.ResolveGatewayClarify(clarifyID, response)
	}()
}

func TestAmbiguousSendReachesWaitForResponse(t *testing.T) {
	env := armedClarify(t, "cid123", 600)
	answerLater("cid123", "user picked B", (clarifySendAckWindow+1)*time.Second)
	response, answered := clarifySendThenWait(context.Background(), env, newConcurrentFuture(), "cid123", "sk", nil)
	if response != "user picked B" || !answered {
		t.Fatalf("got %q %v", response, answered)
	}
}

func TestSentReachesWaitForResponse(t *testing.T) {
	env := armedClarify(t, "cid123", 600)
	answerLater("cid123", "answer", 50*time.Millisecond)
	response, answered := clarifySendThenWait(context.Background(), env, settledFuture(gateway.SendResult{Success: true}, nil), "cid123", "sk", nil)
	if response != "answer" || !answered {
		t.Fatalf("got %q %v", response, answered)
	}
}

func TestDefinitiveFailureNeverWaits(t *testing.T) {
	env := armedClarify(t, "cid123", 600)
	fut := settledFuture(gateway.SendResult{Success: false, Error: "relay prompt op unavailable"}, nil)
	start := time.Now()
	response, answered := clarifySendThenWait(context.Background(), env, fut, "cid123", "sk", nil)
	if response != clarifyUndelivered || answered {
		t.Fatalf("got %q %v", response, answered)
	}
	if time.Since(start) > 5*time.Second || tools.HasPendingClarify("sk") {
		t.Fatal("definitive failure waited or kept the registration")
	}
}

func TestNoResponseReturnsTimeoutSentinel(t *testing.T) {
	env := armedClarify(t, "cid123", 600)
	answerLater("cid123", "", 50*time.Millisecond)
	response, answered := clarifySendThenWait(context.Background(), env, settledFuture(gateway.SendResult{Success: true}, nil), "cid123", "sk", nil)
	if response != "[user did not respond within 10m]" || answered {
		t.Fatalf("got %q %v", response, answered)
	}
}
