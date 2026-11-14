package cron

import "testing"

func TestNormalizeDeliverValueFlattensListsAndDefaultsToLocal(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, "local"}, {"", "local"}, {"telegram", "telegram"}, {"telegram,discord", "telegram,discord"},
		{[]any{"telegram"}, "telegram"}, {[]any{" telegram ", "", "discord"}, "telegram,discord"},
		{[]any{}, "local"}, {[]any{" ", ""}, "local"}, {[]string{"slack"}, "slack"}, {(*string)(nil), "local"},
	}
	for _, c := range cases {
		if got := normalizeDeliverValue(c.in); got != c.want {
			t.Fatalf("normalize(%#v) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestDeliveryLaneValueReadsTheFailureLaneOnlyForFailures(t *testing.T) {
	job := &Job{Deliver: strPtr("telegram"), FailureDeliver: strPtr("discord")}
	if deliveryLaneValue(job, false) != "telegram" || deliveryLaneValue(job, true) != "discord" {
		t.Fatalf("lanes = %v %v", deliveryLaneValue(job, false), deliveryLaneValue(job, true))
	}
	job.FailureDeliver = strPtr("   ")
	if deliveryLaneValue(job, true) != "telegram" {
		t.Fatal("a blank failure lane falls back to deliver")
	}
	if got := normalizeDeliverValue(deliveryLaneValue(&Job{}, true)); got != "local" {
		t.Fatalf("missing deliver = %q", got)
	}
}

func TestParseBotChatDeliverToken(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"bot-chat", "", true}, {" BOT-CHAT ", "", true}, {"bot-chat:Work", "Work", true},
		{"Bot-Chat: coder ", "coder", true}, {"telegram", "", false}, {"", "", false}, {"bot-chatter", "", false},
	}
	for _, c := range cases {
		if got, ok := parseBotChatDeliverToken(c.in); got != c.want || ok != c.ok {
			t.Fatalf("parse(%q) = %q %v", c.in, got, ok)
		}
	}
}
