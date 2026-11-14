package telegram

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// About 9.5k characters of words, three MarkdownV2 chunks
func threeChunkText() string {
	lines := make([]string, 0, 80)
	for i := range 80 {
		words := make([]string, 0, 20)
		for j := range 20 {
			words = append(words, fmt.Sprintf("w%d", i*20+j))
		}
		lines = append(lines, strings.Join(words, " "))
	}
	return strings.Join(lines, "\n")
}

func TestOverCapFloodReturnsPartialOverflowAndArmsCooldown(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	a.richSendDisabled.Store(true)
	var mu sync.Mutex
	n := 0
	fake.respond = func(method string, _ map[string]string) (int, string) {
		if method != "sendMessage" {
			return 0, ""
		}
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 2 {
			return 429, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 120","parameters":{"retry_after":120}}`
		}
		return 200, fmt.Sprintf(`{"ok":true,"result":{"message_id":%d,"date":0,"chat":{"id":4242,"type":"private"}}}`, 1000+n)
	}
	content := threeChunkText()
	ctx := context.Background()
	result, err := a.SendWithRetry(ctx, "4242", content, "", nil, 2, 2.0)
	if err != nil || result.Success || result.Error != "flood_control:120.0" {
		t.Fatalf("result %+v err %v", result, err)
	}
	raw, _ := result.RawResponse.(map[string]any)
	total := raw["total_chunks"].(int)
	if raw["partial_overflow"] != true || raw["delivered_chunks"] != 1 || total < 3 || raw["last_message_id"] != "1001" ||
		len(tupleStrings(raw["undelivered_chunks"])) != total-1 {
		t.Fatalf("raw %+v", raw)
	}
	if len(fake.callsOf("sendMessage")) != 2 {
		t.Fatalf("calls %d", len(fake.callsOf("sendMessage")))
	}
	again, err := a.Send(ctx, "4242", "hello again", "", nil)
	if err != nil || again.Success || !strings.HasPrefix(fmt.Sprint(again.Error), "flood_control:") || len(fake.callsOf("sendMessage")) != 2 {
		t.Fatalf("again %+v", again)
	}
	other, err := a.Send(ctx, "999", "hello other", "", nil)
	if err != nil || !other.Success || len(fake.callsOf("sendMessage")) != 3 {
		t.Fatalf("other %+v", other)
	}
}

func TestConcurrentSplitSendsToOneChatDoNotInterleave(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	a.richSendDisabled.Store(true)
	long := func(tag string) string {
		lines := make([]string, 0, 90)
		for range 90 {
			lines = append(lines, strings.TrimSpace(strings.Repeat(tag+" ", 30)))
		}
		return strings.Join(lines, "\n")
	}
	var wg sync.WaitGroup
	for _, tag := range []string{"REPORT", "ALERT"} {
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			if _, err := a.Send(context.Background(), "1", long(tag), "", nil); err != nil {
				t.Error(err)
			}
		}(tag)
	}
	wg.Wait()
	calls := fake.callsOf("sendMessage")
	var order []string
	for _, c := range calls {
		order = append(order, strings.Fields(c.params["text"])[0])
	}
	if len(order) < 4 {
		t.Fatalf("order %v", order)
	}
	switches := 0
	for i := 1; i < len(order); i++ {
		if order[i] != order[i-1] {
			switches++
		}
	}
	if switches != 1 {
		t.Fatalf("interleaved %v", order)
	}
}
