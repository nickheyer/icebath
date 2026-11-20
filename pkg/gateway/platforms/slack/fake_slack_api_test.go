package slack

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway/platforms/slack/slacktest"
)

type fakeCall = slacktest.Call

// Loopback Slack of the package cases, the shared test kit under the case files' method names
type fakeSlackAPI struct{ *slacktest.Server }

// Fake API every Slack host resolves to for the duration of the test
func newFakeSlackAPI(t *testing.T) *fakeSlackAPI {
	f := &fakeSlackAPI{slacktest.New(t)}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return f
}

// Queues a payload for the method, the last queued payload repeating
func (f *fakeSlackAPI) respond(method string, payload map[string]any) { f.Respond(method, payload) }

// Calls recorded for the method in order
func (f *fakeSlackAPI) callsTo(method string) []fakeCall { return f.CallsTo(method) }

// Every recorded call in order
func (f *fakeSlackAPI) allCalls() []fakeCall { return f.AllCalls() }
