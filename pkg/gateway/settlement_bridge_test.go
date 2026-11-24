package gateway

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Restart notice the owner's settlement writes, for the external cases of the package
const RestartInterruptNoticeForTests = restartInterruptNotice

// Claimed admission of principal on a real store, for the external cases that drive the agent closer
func ClaimedOnRealStoreForTests(t *testing.T, principal string) (context.Context, *state.SessionDB, *SessionAuthority, *LiveSession, *jsonutil.Object) {
	t.Helper()
	return claimedOnRealStore(t, principal, nil)
}

// Settles the claimed row of the test session through the owner's commit and publish
func CommitAndPublishForTests(ctx context.Context, a *SessionAuthority, live *LiveSession, row *jsonutil.Object, response any, outcome string, captured *jsonutil.Object) error {
	return CommitAndPublish(ctx, a, live, testRef, row, response, outcome, captured, &TurnSettlement{})
}

// Failed turn boundary contents of the test session's transcript
func FailedTurnNoticesForTests(t *testing.T, ctx context.Context, db *state.SessionDB) []string {
	t.Helper()
	return failedTurnNotices(t, ctx, db)
}
