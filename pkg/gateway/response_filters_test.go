package gateway

import (
	"strings"
	"testing"
)

func TestExactSilenceTokensAreIntentionalSilence(t *testing.T) {
	for _, token := range []string{"[SILENT]", " SILENT ", "NO_REPLY", "no reply"} {
		if !IsIntentionalSilenceResponse(token) {
			t.Errorf("%q not silence", token)
		}
	}
	if IsIntentionalSilenceResponse("") || IsIntentionalSilenceResponse(nil) || IsIntentionalSilenceResponse("NO_REPLY is my answer") {
		t.Fatal("non marker read as silence")
	}
}

func TestAutonomousSilenceAcceptsMarkerWithOwnLineNote(t *testing.T) {
	for _, s := range []string{"[SILENT]", "[SILENT]\n\nNothing new this tick.", "2 deals filtered\n\n[SILENT]",
		"no_reply\nduplicate inbound, already handled", "[SILENT] No changes detected"} {
		if !IsAutonomousSilenceResponse(s) {
			t.Errorf("%q not silence", s)
		}
	}
}

func TestTranslatedSentinelIsSilenceInEveryFormTheEnglishOneIs(t *testing.T) {
	if !IsIntentionalSilenceResponse("[静默]") || !IsIntentionalSilenceResponse("**沉默**") {
		t.Fatal("translated exact marker")
	}
	for _, s := range []string{"[静默]\n\nNothing new this tick.", "2 deals filtered\n\n[沉默]", "静默"} {
		if !IsAutonomousSilenceResponse(s) {
			t.Errorf("%q not silence", s)
		}
	}
}

func TestProseMentioningTheTranslatedSentinelIsDelivered(t *testing.T) {
	if IsIntentionalSilenceResponse("status: 静默 means the lane is quiet") ||
		IsAutonomousSilenceResponse("the lane said 静默 mid-sentence and kept talking") {
		t.Fatal("prose suppressed")
	}
}

func TestAutonomousLaneAgreesWithInteractiveLaneOnCJKPunctuationVariants(t *testing.T) {
	for _, v := range []string{"【静默】", "静默。", "【沉默】", "沉默。", "**[静默]**", "NO_REPLY."} {
		if !IsIntentionalSilenceResponse(v) || IsAutonomousSilenceResponse(v) != IsIntentionalSilenceResponse(v) {
			t.Errorf("%q lanes disagree", v)
		}
	}
}

func TestIntentionalSilenceAgentResultNeedsSuccess(t *testing.T) {
	if !IsIntentionalSilenceAgentResult(map[string]any{}, "NO_REPLY") || IsIntentionalSilenceAgentResult(map[string]any{"failed": true}, "NO_REPLY") ||
		IsIntentionalSilenceAgentResult(nil, "NO_REPLY") {
		t.Fatal("agent result gate")
	}
}

func TestTrailingSilenceMarkerAndStreamHold(t *testing.T) {
	if got := StripTrailingSilenceMarker("Here is the answer.\n\nNO_REPLY"); got != "Here is the answer." {
		t.Fatalf("strip %q", got)
	}
	if got := StripTrailingSilenceMarker("NO_REPLY"); got != "NO_REPLY" {
		t.Fatalf("bare %q", got)
	}
	if !EndsWithPartialSilenceMarker("answer\n\nNO_RE") || EndsWithPartialSilenceMarker("NO_RE") {
		t.Fatal("partial closing marker")
	}
	var h SilenceMarkerStreamHold
	var shown []string
	for _, d := range []string{"NO", "_RE", "PLY"} {
		shown = append(shown, h.Feed(d))
	}
	if strings.Join(shown, "") != "" || h.Finish() != "" {
		t.Fatalf("bare marker leaked %q", shown)
	}
	var g SilenceMarkerStreamHold
	first := g.Feed("Answer ✓\n\nNO")
	out := first + g.Feed(" problem")
	if first != "Answer ✓" || out != "Answer ✓\n\nNO problem" || g.Finish() != "" {
		t.Fatalf("held line %q then %q", first, out)
	}
	var k SilenceMarkerStreamHold
	if k.Feed("Done\n\nNO_REP") != "Done" || k.Feed("LY") != "" || k.Finish() != "" {
		t.Fatal("closing marker leaked")
	}
}

func TestSilenceAllowedAndReplyExpectedMetadata(t *testing.T) {
	no, yes := false, true
	if !SilenceAllowed("process_complete", nil, nil, nil) || !SilenceAllowed(nil, &no, nil, nil) || SilenceAllowed(nil, nil, &yes, nil) ||
		!SilenceAllowed(nil, nil, &yes, map[string]any{"completed": true}) || SilenceAllowed(nil, nil, &yes, map[string]any{"completed": false}) ||
		SilenceAllowed(nil, nil, &yes, map[string]any{"partial": 1}) {
		t.Fatal("silence allowed")
	}
	if len(ReplyExpectedMetadata(nil)) != 0 || ReplyExpectedMetadata(&no)["reply_expected"] != false {
		t.Fatal("reply expected metadata")
	}
}

func TestCodeSkewDetectsDriftSinceBoot(t *testing.T) {
	prev := codeSkewFingerprint
	fp, ok := "git:refs/heads/main:0123456789abcdef", true
	codeSkewFingerprint = func() (string, bool) { return fp, ok }
	bootFingerprint = nil
	defer func() { codeSkewFingerprint = prev; bootFingerprint = nil }()
	if _, _, skew := DetectCodeSkew(); skew {
		t.Fatal("skew before boot snapshot")
	}
	RecordBootFingerprint()
	if _, _, skew := DetectCodeSkew(); skew {
		t.Fatal("skew without drift")
	}
	fp = "git:refs/heads/main:fedcba9876543210"
	if b, d, skew := DetectCodeSkew(); !skew || b != "0123456789" || d != "fedcba9876" {
		t.Fatalf("skew %v %s %s", skew, b, d)
	}
	if sha, ok := CurrentCodeSHA(); !ok || sha != "fedcba9876543210" {
		t.Fatalf("sha %s", sha)
	}
	fp = "git:refs/heads/main:unresolved"
	if _, ok := CurrentCodeSHA(); ok {
		t.Fatal("unresolved sha")
	}
	if b, d, skew := DetectCodeSkew(); !skew || b != "0123456789" || d != "unresolved" {
		t.Fatalf("unresolved skew %s %s", b, d)
	}
	ok = false
	if _, _, skew := DetectCodeSkew(); skew {
		t.Fatal("unreadable checkout reported skew")
	}
}
