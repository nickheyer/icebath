package agent

import "testing"

func TestRowsToMessagesAndSessionMessageDictsKeepRolesAndContent(t *testing.T) {
	rows := []map[string]any{{"role": "user", "content": "hello"}, {"role": "assistant", "content": "hi there"}}
	msgs := RowsToMessages(rows)
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("got %+v", msgs)
	}
	dicts := SessionMessageDicts(msgs)
	if len(dicts) != 2 || dicts[0]["role"] != "user" || dicts[1]["content"] != "hi there" {
		t.Fatalf("got %v", dicts)
	}
	if len(RowsToMessages(nil)) != 0 {
		t.Fatal("no rows must give no messages")
	}
}
