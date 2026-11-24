package gateway

import (
	"math/big"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

const (
	// Gateway authored grant of one more budget to a discussion paused at its cap
	continuedEventKind = "room.continued"
	// Continues one discussion may take, each another full budget of rounds and messages
	MaxDiscussionContinues = 5
)

var continuedFields = []string{"discussion_event_id", "thread_id", "grant"}

// Continues the log grants one discussion
func discussionContinueCount(events []discussionEvent, discussionEventID string) int64 {
	var count int64
	for _, event := range events {
		if event.kind == continuedEventKind && event.payload["discussion_event_id"] == discussionEventID {
			count++
		}
	}
	return count
}

// Round after the last reply before the latest Continue, false without a Continue
func discussionResumeRound(events []discussionEvent, discussionEventID string, memberMessages []discussionEvent) (int64, bool) {
	var latest int64
	continued := false
	for _, event := range events {
		if event.kind == continuedEventKind && event.payload["discussion_event_id"] == discussionEventID {
			latest, continued = max(latest, event.seq), true
		}
	}
	if !continued {
		return 0, false
	}
	last := int64(-1)
	for _, event := range memberMessages {
		if event.seq < latest {
			round, _ := hostedStrictInt(event.payload["round_index"])
			last = max(last, round)
		}
	}
	return last + 1, true
}

// Seq of the latest Continue of each discussion, activity before it no longer completing the discussion
func discussionContinuedThrough(events []discussionEvent) map[string]int64 {
	latest := map[string]int64{}
	for _, event := range events {
		if event.kind == continuedEventKind {
			latest[anyutil.String(event.payload["discussion_event_id"])] = event.seq
		}
	}
	return latest
}

// Validated room.continued payload from the room gateway
func discussionValidateContinued(kind string, payload, actor map[string]any, room DiscussionRoom) (map[string]any, error) {
	if _, err := discussionExactFields(payload, kind+" payload", continuedFields); err != nil {
		return nil, err
	}
	if err := discussionRequireGateway(actor, room, kind+" requires the room gateway"); err != nil {
		return nil, err
	}
	for _, field := range []string{"discussion_event_id", "thread_id"} {
		if _, err := discussionIdentifier(payload[field], field); err != nil {
			return nil, err
		}
	}
	if _, err := roomBoundedInt(payload["grant"], discussionInvalid, kind+" grant must be a positive integer", 1, big.NewInt(MaxDiscussionContinues)); err != nil {
		return nil, err
	}
	return payload, nil
}

// Thread of a discussion paused at its cap and the number its next Continue takes
type pausedContinue struct {
	threadID any
	grant    int64
}

// Pause of a discussion whose latest outcome is the cap, nil once a Stop or a newer message on its thread followed its message
func pausedDiscussion(events []map[string]any, discussionEventID string) (*pausedContinue, error) {
	var bounded map[string]any
	var grants int64
	for _, event := range events {
		payload, _ := event["payload"].(map[string]any)
		if payload["discussion_event_id"] != discussionEventID {
			continue
		}
		switch event["kind"] {
		case continuedEventKind:
			grants, bounded = grants+1, nil
		case "room.activity":
			bounded = nil
			if payload["status"] == "bounded" {
				bounded = event
			}
		}
	}
	if bounded == nil {
		return nil, nil
	}
	threadID := bounded["payload"].(map[string]any)["thread_id"]
	sourceSeq, err := anyutil.ToInt(bounded["seq"])
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if event["event_id"] == discussionEventID {
			if sourceSeq, err = anyutil.ToInt(event["seq"]); err != nil {
				return nil, err
			}
			break
		}
	}
	for _, event := range events {
		seq, err := anyutil.ToInt(event["seq"])
		if err != nil {
			return nil, err
		}
		if seq <= sourceSeq {
			continue
		}
		payload, _ := event["payload"].(map[string]any)
		if event["kind"] == "room.stop_requested" || (event["kind"] == "message.user" && anyutil.Equal(payload["thread_id"], threadID)) {
			return nil, nil
		}
	}
	return &pausedContinue{threadID: threadID, grant: grants + 1}, nil
}
