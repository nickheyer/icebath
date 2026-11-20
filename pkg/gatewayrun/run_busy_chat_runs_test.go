package gatewayrun

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Discord forum thread source of one participant
func threadSource(uid string) *gateway.SessionSource {
	return &gateway.SessionSource{Platform: gateway.PlatformDiscord, ChatType: "forum", ChatID: "chan1", ThreadID: strp("thr1"), UserID: strp(uid)}
}

// Session key of a source under the given per user modes and profile
func siblingKey(t *testing.T, source *gateway.SessionSource, threadPerUser bool, profile string) string {
	t.Helper()
	key, err := gateway.BuildSessionKey(context.Background(), source, true, threadPerUser, profile)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Bare runner whose only running turn is a real agent under key
func runnerRunning(t *testing.T, key string) *GatewayRunner {
	r := allocGatewayRunner()
	r.sessions = map[string]*SessionState{}
	r.runningAgents().Set(key, quietAgent(t))
	return r
}

// Sibling thread keys of the caller's chat runs
func siblings(t *testing.T, r *GatewayRunner, source *gateway.SessionSource, ownKey string) []string {
	t.Helper()
	runs, err := r.sameChatRuns(context.Background(), source, ownKey)
	if err != nil {
		t.Fatal(err)
	}
	return siblingThreadRunKeys(source, runs)
}

func TestSiblingReturnsEmptyForNonThreadSource(t *testing.T) {
	nonthread := &gateway.SessionSource{Platform: gateway.PlatformDiscord, ChatType: "group", ChatID: "chan1", UserID: strp("userA")}
	grpB := siblingKey(t, &gateway.SessionSource{Platform: gateway.PlatformDiscord, ChatType: "group", ChatID: "chan1", UserID: strp("userB")}, false, "")
	r := runnerRunning(t, grpB)
	if got := siblings(t, r, nonthread, "agent:main:discord:group:chan1:userA"); len(got) != 0 {
		t.Fatalf("siblings %v", got)
	}
}

func TestSiblingMatchesNamedProfileRuns(t *testing.T) {
	source := threadSource("userA")
	source.Profile = strp("work")
	keyB := siblingKey(t, threadSource("userB"), true, "work")
	r := runnerRunning(t, keyB)
	if got := siblings(t, r, source, "agent:work:discord:forum:chan1:thr1:userA"); !reflect.DeepEqual(got, []string{keyB}) {
		t.Fatalf("siblings %v want %v", got, keyB)
	}
}

func TestSiblingDoesNotCrossProfiles(t *testing.T) {
	source := threadSource("userA")
	source.Profile = strp("work")
	mainKey := siblingKey(t, threadSource("userB"), true, "")
	r := runnerRunning(t, mainKey)
	if got := siblings(t, r, source, "agent:work:discord:forum:chan1:thr1:userA"); len(got) != 0 {
		t.Fatalf("siblings %v", got)
	}
}
