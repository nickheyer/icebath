package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func continuationConsumer() *GatewayStreamConsumer {
	cfg := DefaultStreamConsumerConfig()
	cfg.Cursor = " ▉"
	return NewGatewayStreamConsumer(newStreamRecorder(false), "chat", StreamConsumerOptions{Config: cfg})
}

func TestMidWordPrefixBacksUpToLastSpace(t *testing.T) {
	c := continuationConsumer()
	c.lastSentText = "Правлю точними живими рядкам ▉"
	got := c.continuationText("Правлю точними живими рядками (мій попередній EN-паттерн…")
	if got != "рядками (мій попередній EN-паттерн…" {
		t.Fatalf("continuation = %q", got)
	}
}

func TestOneLongTokenKeepsOriginalCut(t *testing.T) {
	c := continuationConsumer()
	c.lastSentText = strings.Repeat("у", 100)
	if got := c.continuationText(strings.Repeat("у", 100) + strings.Repeat("д", 20)); got != strings.Repeat("д", 20) {
		t.Fatalf("continuation = %q", got)
	}
}

func TestWholeWordPrefixIsNotResent(t *testing.T) {
	c := continuationConsumer()
	c.lastSentText = "Here is the answer ▉"
	if got := c.continuationText("Here is the answer to your question."); got != "to your question." {
		t.Fatalf("continuation = %q", got)
	}
}

func TestPartialPredicateAgreesWithExactOnFullMarkers(t *testing.T) {
	for marker := range LiveGatewaySilentMarkers {
		if !IsPartialSilenceMarker(marker) || !IsIntentionalSilenceResponse(marker) {
			t.Fatalf("marker %q", marker)
		}
	}
}

// Deleting recorder whose sends get fresh numbered ids
func numberedDeleter() *streamDeleter {
	adapter := newStreamDeleter()
	n := 0
	adapter.sendFn = func(string) (SendResult, error) {
		n++
		return SendResult{Success: true, MessageID: fmt.Sprintf("m%d", n)}, nil
	}
	return adapter
}

// Message contents still on screen after sends, edits and deletes
func visibleMessages(a *streamDeleter) []string {
	order := []string{}
	shown := map[string]string{}
	sends := 0
	for _, call := range a.calls {
		switch call.op {
		case "send":
			sends++
			id := fmt.Sprintf("m%d", sends)
			order = append(order, id)
			shown[id] = call.content
		case "edit":
			shown[call.messageID] = call.content
		}
	}
	for _, d := range a.deleted {
		delete(shown, d[1])
	}
	var out []string
	for _, id := range order {
		if text, ok := shown[id]; ok {
			out = append(out, text)
		}
	}
	return out
}

func silenceConfig() *StreamConsumerConfig {
	cfg := DefaultStreamConsumerConfig()
	cfg.EditInterval, cfg.BufferThreshold = 0.01, 1
	return cfg
}

func TestNoReplyOnlyStreamIsFullySuppressed(t *testing.T) {
	adapter := newStreamDeleter()
	adapter.sendFn = sendSequence(SendResult{Success: true, MessageID: "preview_1"})
	consumer := NewGatewayStreamConsumer(adapter, "chat_1", StreamConsumerOptions{Config: silenceConfig()})
	consumer.OnDelta("NO_REPLY")
	consumer.Finish(nil)
	consumer.Run(context.Background())
	for _, call := range adapter.calls {
		if strings.Contains(call.content, "NO_REPLY") {
			t.Fatalf("marker leaked: %q", call.content)
		}
	}
	if consumer.finalResponseSent || consumer.finalContentDelivered || consumer.alreadySent {
		t.Fatal("silent stream set delivery flags")
	}
}

func TestPartialMarkerPreviewIsRetracted(t *testing.T) {
	adapter := newStreamDeleter()
	consumer := NewGatewayStreamConsumer(adapter, "chat_1", StreamConsumerOptions{Config: silenceConfig()})
	consumer.messageID, consumer.previewMessageIDs, consumer.alreadySent = "preview_1", idSet{"preview_1"}, true
	consumer.OnDelta("NO_REPLY")
	consumer.Finish(nil)
	consumer.Run(context.Background())
	if len(adapter.deleted) != 1 || adapter.deleted[0] != [2]string{"chat_1", "preview_1"} {
		t.Fatalf("deleted = %v", adapter.deleted)
	}
	if consumer.finalContentDelivered || consumer.alreadySent {
		t.Fatal("retracted preview kept delivery flags")
	}
}

const silenceAnswer = "Your meeting is at 3pm."

func streamAnswerThenMarker(finalText string) (*streamDeleter, *GatewayStreamConsumer) {
	adapter := numberedDeleter()
	consumer := NewGatewayStreamConsumer(adapter, "chat_1", StreamConsumerOptions{Config: silenceConfig()})
	consumer.OnDelta(silenceAnswer)
	consumer.OnDelta(nil)
	consumer.OnDelta("NO_REPLY")
	consumer.Finish(&finalText)
	consumer.Run(context.Background())
	return adapter, consumer
}

func TestReplyFinalAfterMarkerShowsTheAnswerOnce(t *testing.T) {
	adapter, consumer := streamAnswerThenMarker(silenceAnswer)
	if got := visibleMessages(adapter); strings.Join(got, "|") != silenceAnswer {
		t.Fatalf("visible = %q", got)
	}
	if len(adapter.deleted) != 0 {
		t.Fatalf("deleted = %v", adapter.deleted)
	}
	for _, call := range adapter.calls {
		if strings.Contains(call.content, "NO_REPLY") {
			t.Fatalf("marker leaked: %q", call.content)
		}
	}
	if !consumer.HasDurablyDeliveredText(silenceAnswer) {
		t.Fatal("answer not durably delivered")
	}
}

func TestMarkerFinalStillRetractsTheTurn(t *testing.T) {
	adapter, consumer := streamAnswerThenMarker("NO_REPLY")
	if got := visibleMessages(adapter); len(got) != 0 {
		t.Fatalf("visible = %q", got)
	}
	if consumer.finalContentDelivered {
		t.Fatal("silent turn marked content delivered")
	}
}
