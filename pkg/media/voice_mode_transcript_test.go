package media

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Replaces the stop phrase loader for one test
func withStopPhrases(t *testing.T, phrases []string) {
	t.Helper()
	orig := loadVoiceStopPhrases
	loadVoiceStopPhrases = func() []string { return phrases }
	t.Cleanup(func() { loadVoiceStopPhrases = orig })
}

// Replaces the voice config section for one test
func withVoiceConfig(t *testing.T, cfg map[string]any) {
	t.Helper()
	orig := voiceConfig
	voiceConfig = func(context.Context) map[string]any { return cfg }
	t.Cleanup(func() { voiceConfig = orig })
}

func TestVoiceStopHintDisabledPhrasesShowNoHint(t *testing.T) {
	withStopPhrases(t, []string{})
	if got := voiceStopHint(); got != "" {
		t.Fatalf("hint %q", got)
	}
}

func TestIsVoiceStopPhraseBareStopMatches(t *testing.T) {
	for _, u := range []string{"stop", "Stop", "STOP", "stop.", "Stop!", " stop ", `"Stop."`, "stop?"} {
		if !isVoiceStopPhrase(u, []string{"stop"}) {
			t.Fatalf("%q did not match", u)
		}
	}
}

func TestIsVoiceStopPhraseUsesConfigWhenPhrasesOmitted(t *testing.T) {
	withStopPhrases(t, []string{"halt"})
	if !isVoiceStopPhrase("halt", nil) || isVoiceStopPhrase("stop", nil) {
		t.Fatal("configured phrases not honored")
	}
}

func TestLoadVoiceStopPhrasesDefault(t *testing.T) {
	withVoiceConfig(t, map[string]any{})
	if got := loadVoiceStopPhrases(); !reflect.DeepEqual(got, defaultVoiceStopPhrases) {
		t.Fatalf("got %v", got)
	}
}

func TestLoadVoiceStopPhrasesConfigErrorFallsBack(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("voice: [unclosed\n  : :"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadVoiceStopPhrases(); !reflect.DeepEqual(got, defaultVoiceStopPhrases) {
		t.Fatalf("got %v", got)
	}
}

func TestIsTtsEcho(t *testing.T) {
	t.Run("test_near_verbatim_repeat_is_echo", func(t *testing.T) {
		spoken := "맞아요. 사용자가 마이크를 끄는 게 아니라 앱이 제 음성은 " + "에코 제거로 걸러내고 실제 사용자 음성만 끼어들기로 받아야 해요."
		if !isTTSEcho(spoken, spoken, defaultTTSEchoSimilarityThreshold) {
			t.Fatal("not echo")
		}
	})
	t.Run("test_repeat_with_leading_stutter_is_echo", func(t *testing.T) {
		if !isTTSEcho("네 방금 네 방금도 제 답변이 그대로 다시 입력됐어요.", "네, 방금도 제 답변이 그대로 다시 입력됐어요.", defaultTTSEchoSimilarityThreshold) {
			t.Fatal("not echo")
		}
	})
	t.Run("test_unrelated_interjection_is_not_echo", func(t *testing.T) {
		if isTTSEcho("actually can you also check my calendar for tomorrow", "The weather today is sunny with a light breeze from the west.", defaultTTSEchoSimilarityThreshold) {
			t.Fatal("echo")
		}
	})
	t.Run("test_short_unrelated_reply_is_not_echo", func(t *testing.T) {
		if isTTSEcho("stop", "I've finished summarizing the document you shared earlier.", defaultTTSEchoSimilarityThreshold) {
			t.Fatal("echo")
		}
	})
	t.Run("test_short_fragment_of_longer_multi_sentence_reply_is_echo", func(t *testing.T) {
		spoken := "Sure, here's a summary of what we found. The build failed " +
			"because of a missing dependency in the lockfile. I've already " +
			"gone ahead and regenerated it, and the tests are passing " +
			"again locally. Let me know if you'd like me to open a PR for " +
			"this or if you want to review the diff first before I do " +
			"anything else."
		if !isTTSEcho("Sure, here's a summary of what we found.", spoken, defaultTTSEchoSimilarityThreshold) {
			t.Fatal("not echo")
		}
	})
	t.Run("test_short_fragment_from_middle_of_reply_is_echo", func(t *testing.T) {
		spoken := "The deployment finished successfully. All three services " +
			"came up healthy, and the smoke tests passed without any " + "errors."
		if !isTTSEcho("the smoke tests passed without any errors", spoken, defaultTTSEchoSimilarityThreshold) {
			t.Fatal("not echo")
		}
	})
	t.Run("test_short_genuine_acknowledgement_is_not_echo", func(t *testing.T) {
		if isTTSEcho("yes", "Yes, I can help with that -- let me pull up the details for you.", defaultTTSEchoSimilarityThreshold) {
			t.Fatal("echo")
		}
	})
	t.Run("test_short_fragment_of_longer_reply_in_no_whitespace_language_is_echo", func(t *testing.T) {
		spoken := "部署已经成功完成。所有三个服务都正常运行状态良好,冒烟测试也全部通过,没有发现任何错误。"
		if !isTTSEcho("所有三个服务都正常运行状态良好", spoken, defaultTTSEchoSimilarityThreshold) {
			t.Fatal("not echo")
		}
	})
	t.Run("test_empty_inputs_are_not_echo", func(t *testing.T) {
		if isTTSEcho("", "hello", 0.6) || isTTSEcho("hello", "", 0.6) || isTTSEcho("", "", 0.6) {
			t.Fatal("echo")
		}
	})
	t.Run("test_case_and_whitespace_insensitive", func(t *testing.T) {
		if !isTTSEcho("  SURE,   I can help with that   right away.  ", "Sure, I can help with that right away.", defaultTTSEchoSimilarityThreshold) {
			t.Fatal("not echo")
		}
	})
	t.Run("test_custom_threshold_is_honored", func(t *testing.T) {
		spoken := "This is a moderately similar sentence about testing."
		transcript := "This is a rather different sentence about coding."
		if !isTTSEcho(transcript, spoken, 0.5) || isTTSEcho(transcript, spoken, 0.95) {
			t.Fatal("threshold ignored")
		}
	})
}
