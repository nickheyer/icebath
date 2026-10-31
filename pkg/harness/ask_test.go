package harness

import (
	"context"
	"testing"
	"time"
)

func TestQuestionerAnswers(t *testing.T) {
	var msgs []any
	q := NewQuestioner(func(msg any) { msgs = append(msgs, msg) })
	qs := []Question{{Text: "which", Choices: []Choice{{Label: "a"}, {Label: "b"}}}, {Text: "how", Choices: []Choice{{Label: "x"}, {Label: "y"}}, Multi: true}}
	got := make(chan []Answer, 1)
	go func() {
		answers, err := q.Ask(context.Background(), "1", "sess", qs)
		if err != nil {
			t.Error(err)
		}
		got <- answers
	}()
	var asking *Asking
	for deadline := time.Now().Add(2 * time.Second); asking == nil && time.Now().Before(deadline); {
		if a := q.ForSession("sess"); a != nil {
			asking = a
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if asking == nil || len(q.Pending()) != 1 || q.Len() != 1 || q.ForSession("other") != nil {
		t.Fatalf("pending: %+v", q.Pending())
	}
	if q.Answer(asking.ID, []Answer{{Picked: []string{"a"}}}) {
		t.Fatal("answered with too few answers")
	}
	if q.Answer(99, []Answer{{}, {}}) {
		t.Fatal("answered an unknown ask")
	}
	if !q.Answer(asking.ID, []Answer{{Picked: []string{"a"}}, {Text: "typed"}}) {
		t.Fatal("answer refused")
	}
	answers := <-got
	if len(answers) != 2 || answers[0].String() != "a" || answers[1].String() != "typed" || q.Len() != 0 {
		t.Fatalf("answers %+v", answers)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages %+v", msgs)
	}
	if _, ok := msgs[0].(QuestionMsg); !ok {
		t.Fatalf("first message %T", msgs[0])
	}
	if done, ok := msgs[1].(QuestionDoneMsg); !ok || done.ID != asking.ID || done.Session != "sess" || done.Err != nil {
		t.Fatalf("second message %+v", msgs[1])
	}
}

func TestQuestionerDeclinesAndCloses(t *testing.T) {
	q := NewQuestioner(func(any) {})
	qs := []Question{{Text: "which", Choices: []Choice{{Label: "a"}, {Label: "b"}}}}
	got := make(chan []Answer, 1)
	go func() {
		answers, _ := q.Ask(context.Background(), "1", "sess", qs)
		got <- answers
	}()
	for q.Len() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if !q.Decline(q.Pending()[0].ID) {
		t.Fatal("decline refused")
	}
	if answers := <-got; len(answers) != 1 || !answers[0].Declined || answers[0].Given() || answers[0].String() != "no answer" {
		t.Fatalf("declined answers %+v", answers)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := q.Ask(ctx, "2", "sess", qs)
		errs <- err
	}()
	for q.Len() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-errs; err != context.Canceled {
		t.Fatalf("cancelled ask: %v", err)
	}
	q.Close()
	if _, err := q.Ask(context.Background(), "3", "sess", qs); err != ErrQuestionsClosed {
		t.Fatalf("ask after close: %v", err)
	}
	if _, err := q.Ask(context.Background(), "3", "sess", nil); err == nil {
		t.Fatal("ask with no questions")
	}
}
