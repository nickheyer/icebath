package provider

import (
	"reflect"
	"testing"
)

// test_bot_relay.py test_delivery_turn_author_from_envelope_sender_fields
func TestDeliveryTurnAuthorFromEnvelopeSenderFields(t *testing.T) {
	if got := DeliveryTurnAuthor("ops", "ops-bot", nil); !reflect.DeepEqual(got, map[string]any{"id": "bot:ops", "name": "ops-bot", "is_bot": true}) {
		t.Fatalf("author %v", got)
	}
	if got := DeliveryTurnAuthor("ops", "", nil); !reflect.DeepEqual(got, map[string]any{"id": "bot:ops", "name": "ops", "is_bot": true}) {
		t.Fatalf("name fallback %v", got)
	}
	if got := DeliveryTurnAuthor("", "ops-bot", nil); got != nil {
		t.Fatalf("no profile %v", got)
	}
	if got := DeliveryTurnAuthor(nil, nil, nil); got != nil {
		t.Fatalf("no sender %v", got)
	}
}

// test_bot_relay.py test_delivery_turn_author_qualifies_a_remote_sender_by_its_connection
func TestDeliveryTurnAuthorQualifiesARemoteSenderByItsConnection(t *testing.T) {
	for _, c := range []struct {
		connection any
		id         string
	}{{"cloud-1", "bot:cloud-1/ops"}, {"local", "bot:local/ops"}, {"", "bot:ops"}} {
		want := map[string]any{"id": c.id, "name": "ops-bot", "is_bot": true}
		if got := DeliveryTurnAuthor("ops", "ops-bot", c.connection); !reflect.DeepEqual(got, want) {
			t.Fatalf("%v: %v", c.connection, got)
		}
	}
}
