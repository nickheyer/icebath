package provider

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func hasConversationTag(tags []string) bool {
	return slices.ContainsFunc(tags, func(t string) bool { return strings.HasPrefix(t, "conversation=") })
}

func TestNousPortalTagsContainsProductAndClient(t *testing.T) {
	tags := nousPortalTags(context.Background(), "")
	if !slices.Contains(tags, "product=icebath-agent") || !slices.Contains(tags, "client=icebath-client-v"+BuildVersion) {
		t.Fatalf("tags %v", tags)
	}
}

func TestPortalAmbientContextSetEmptyClears(t *testing.T) {
	outer := WithPortalConversationContext(context.Background(), "outer")
	ctx := WithPortalConversationContext(outer, "")
	if portalConversationContext(ctx) != "" || hasConversationTag(nousPortalTags(ctx, "")) {
		t.Fatalf("empty id published %q", portalConversationContext(ctx))
	}
	if portalConversationContext(outer) != "outer" {
		t.Fatalf("outer context lost its id")
	}
	scoped := WithPortalAffinityScope(WithPortalAffinityScope(context.Background(), "chat"), "")
	if portalAffinityScope(scoped) != "" {
		t.Fatalf("empty scope published")
	}
}

func TestPortalAmbientContextIsolatedBetweenContexts(t *testing.T) {
	base := context.Background()
	in := func(cid string) []string { return nousPortalTags(WithPortalConversationContext(base, cid), "") }
	a, b := in("agent-a"), in("agent-b")
	if !slices.Contains(a, conversationTag("agent-a")) || !slices.Contains(b, conversationTag("agent-b")) || slices.Contains(a, conversationTag("agent-b")) {
		t.Fatalf("a=%v b=%v", a, b)
	}
	if hasConversationTag(nousPortalTags(base, "")) {
		t.Fatalf("outer context leaked a conversation tag")
	}
}

func TestPortalAmbientContextPropagatesToWorkers(t *testing.T) {
	ctx := WithPortalConversationContext(context.Background(), "moa-root")
	plain, propagated := make(chan []string, 1), make(chan []string, 1)
	go func() { plain <- nousPortalTags(context.Background(), "") }()
	go func(c context.Context) { propagated <- nousPortalTags(c, "") }(ctx)
	if hasConversationTag(<-plain) {
		t.Fatalf("worker without the context carried the tag")
	}
	if !slices.Contains(<-propagated, conversationTag("moa-root")) {
		t.Fatalf("worker given the context lost the tag")
	}
}

func TestPortalConversationContextWinsOverSessionID(t *testing.T) {
	ctx := WithPortalConversationContext(context.Background(), "root")
	tags := nousPortalTags(ctx, "segment")
	if !slices.Contains(tags, conversationTag("root")) || slices.Contains(tags, conversationTag("segment")) {
		t.Fatalf("tags %v", tags)
	}
	if !slices.Contains(nousPortalTags(context.Background(), "segment"), conversationTag("segment")) {
		t.Fatalf("explicit session id dropped outside a turn")
	}
	if portalAffinityScope(WithPortalAffinityScope(ctx, "declared")) != "declared" {
		t.Fatalf("affinity scope not published")
	}
}

func TestNousPortalTagsExportedMatchesUnexported(t *testing.T) {
	ctx := WithPortalConversationContext(context.Background(), "conv-9")
	if got, want := strings.Join(NousPortalTags(ctx, "sess-1"), ","), strings.Join(nousPortalTags(ctx, "sess-1"), ","); got != want || !strings.Contains(got, "conversation=conv-9") {
		t.Fatalf("got %q want %q", got, want)
	}
}
