package tools

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func spokenDefault(text string) string {
	n := SpokenTextDefaultMaxChars
	return PrepareSpokenText(text, &n)
}

func assertSpokenContains(t *testing.T, spoken string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(spoken, w) {
			t.Errorf("%q lost from %q", w, spoken)
		}
	}
}

func assertSpokenOmits(t *testing.T, spoken string, banned ...string) {
	t.Helper()
	for _, b := range banned {
		if strings.Contains(spoken, b) {
			t.Errorf("%q leaked into %q", b, spoken)
		}
	}
}

// Shared identifier corpus silences machine tokens and keeps ordinary speech verbatim
func TestTTSIdentifierSpeech(t *testing.T) {
	raw, err := os.ReadFile("testdata/identifier_speech_corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		IdentifierTokens  [][3]json.RawMessage `json:"identifier_tokens"`
		PassThroughTokens [][3]json.RawMessage `json:"pass_through_tokens"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	decode := func(row [3]json.RawMessage) (string, []string, []string) {
		var text string
		var omit, keep []string
		if json.Unmarshal(row[0], &text) != nil || json.Unmarshal(row[1], &omit) != nil || json.Unmarshal(row[2], &keep) != nil {
			t.Fatalf("bad corpus row %s", row)
		}
		return text, omit, keep
	}
	t.Run("test_identifier_tokens_become_silence_prose_survives", func(t *testing.T) {
		for _, row := range corpus.IdentifierTokens {
			text, omit, keep := decode(row)
			spoken := spokenDefault(text)
			assertSpokenOmits(t, spoken, omit...)
			assertSpokenContains(t, spoken, keep...)
		}
	})
	t.Run("test_ordinary_speech_passes_through", func(t *testing.T) {
		for _, row := range corpus.PassThroughTokens {
			text, _, keep := decode(row)
			assertSpokenContains(t, spokenDefault(text), keep...)
		}
	})
	t.Run("test_issue_repro_filenames_are_not_spoken", func(t *testing.T) {
		spoken := spokenDefault("Saved peyton-sample-20260922.wav and peyton-sample-20260922.ogg.")
		assertSpokenOmits(t, spoken, ".wav", ".ogg", "peyton")
		assertSpokenContains(t, spoken, "Saved")
	})
	t.Run("test_hash_and_uuid_summaries_do_not_spell_out", func(t *testing.T) {
		spoken := spokenDefault("Commit 73688014f78 with trace 550e8400-e29b-41d4-a716-446655440000.")
		assertSpokenOmits(t, spoken, "73688014f78", "550e8400")
		assertSpokenContains(t, spoken, "Commit", "trace")
	})
	t.Run("test_plain_words_with_hyphens_and_digits_stay_spoken", func(t *testing.T) {
		assertSpokenContains(t, spokenDefault("COVID-19 numbers and the well-known 7 habit stay."), "COVID-19", "well-known", "7")
	})
	t.Run("test_dates_and_ratios_are_not_identifiers", func(t *testing.T) {
		assertSpokenContains(t, spokenDefault("Due 2026/06/02; the score was 3:2."), "2026/06/02", "3:2")
	})
}

// Units, headings, currencies, media links and trailing colons become calm spoken sentences
func TestTTSTextNormalize(t *testing.T) {
	t.Run("test_prepare_spoken_text_expands_celsius_and_weather_units", func(t *testing.T) {
		raw := "## Christchurch today\n\n- **Now:** about **14°C**, feels like **14°C**\n- **Wind:** 9 km/h\n- **Rain:** 1.3 mm\n- **Range:** 11–17°C\n"
		spoken := spokenDefault(raw)
		assertSpokenOmits(t, spoken, "##", "**", "°C", "km/h")
		assertSpokenContains(t, spoken, "14 degrees Celsius", "11 to 17 degrees Celsius", "9 kilometres per hour", "1.3 millimetres")
	})
	t.Run("test_prepare_spoken_text_polish_edge_cases", func(t *testing.T) {
		if got := spokenDefault("## Weather\nIt will be sunny"); got != "Weather, It will be sunny." {
			t.Fatalf("heading fold %q", got)
		}
		assertSpokenContains(t, spokenDefault("measured in °C"), "degrees Celsius")
		assertSpokenContains(t, spokenDefault("US$300, next"), "300 US dollars")
		assertSpokenContains(t, spokenDefault("$5/month"), "5 dollars per month")
		assertSpokenContains(t, spokenDefault("choose and/or option"), "and/or")
		assertSpokenContains(t, spokenDefault("status N/A here"), "N/A")
		assertSpokenContains(t, spokenDefault("due 2026/06/02 ok"), "2026/06/02")
	})
	t.Run("test_prepare_spoken_text_strips_media_file_links", func(t *testing.T) {
		spoken := spokenDefault("The files are below.\nMEDIA:/Users/ricardo.mendes/Documents/inference-server-shopping-list.xlsx\nBye.")
		assertSpokenOmits(t, spoken, "MEDIA", "shopping-list", "xlsx")
		assertSpokenContains(t, spoken, "below", "Bye")
	})
	t.Run("test_prepare_spoken_text_keeps_sentence_break_after_inline_media_link", func(t *testing.T) {
		if got := spokenDefault("See MEDIA:/tmp/report-2026-q3.xlsx. Then reply."); got != "See. Then reply." {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_prepare_spoken_text_closes_trailing_colons", func(t *testing.T) {
		spoken := spokenDefault("Here is the list:\nMEDIA:/tmp/x.py\nMore text")
		assertSpokenOmits(t, spoken, "list:")
		assertSpokenContains(t, spoken, "list.")
	})
	t.Run("test_prepare_spoken_text_closes_colon_on_single_line", func(t *testing.T) {
		if got := spokenDefault("Here is the list:"); got != "Here is the list." {
			t.Fatalf("got %q", got)
		}
		if got := spokenDefault("Final score 3:2"); got != "Final score 3:2" {
			t.Fatalf("ratio %q", got)
		}
	})
}

// Think blocks, verifier footers, emoji and newlines never reach the voice on any entry path
func TestTTSPrepareSpoken(t *testing.T) {
	footer := "⚠️ File-mutation verifier: 2 file(s) were NOT modified this turn despite any wording above that may suggest otherwise. Run `git status` or `read_file` to confirm.\n" +
		"  • `tools/foo.py` — [patch] old_string not found\n" +
		"  • `bar.md` — [write_file] failed"
	t.Run("test_think_block_removed", func(t *testing.T) {
		spoken := spokenDefault("<think>\nsecret reasoning here\n</think>\nThe answer is 42.")
		assertSpokenOmits(t, spoken, "secret reasoning")
		assertSpokenContains(t, spoken, "42")
	})
	t.Run("test_think_block_with_attributes_removed", func(t *testing.T) {
		spoken := spokenDefault("<think budget=high>chain of thought</think>Visible.")
		assertSpokenOmits(t, spoken, "chain of thought")
		assertSpokenContains(t, spoken, "Visible")
	})
	t.Run("test_unterminated_think_block_removed", func(t *testing.T) {
		spoken := spokenDefault("Answer first. <think>\ntruncated reasoning stream")
		assertSpokenOmits(t, spoken, "truncated reasoning")
		assertSpokenContains(t, spoken, "Answer first")
	})
	t.Run("test_multiple_think_blocks", func(t *testing.T) {
		spoken := StripNonspokenBlocks("<think>a</think>one<think>b</think> two")
		if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(spoken, "one", ""), "two", ""), "a") {
			t.Fatalf("reasoning leaked %q", spoken)
		}
		assertSpokenContains(t, spoken, "one", "two")
	})
	t.Run("test_footer_removed", func(t *testing.T) {
		spoken := spokenDefault("I fixed the file.\n\n" + footer)
		assertSpokenOmits(t, spoken, "File-mutation verifier", "NOT modified")
		assertSpokenContains(t, spoken, "fixed the file")
	})
	t.Run("test_text_without_footer_untouched", func(t *testing.T) {
		raw := "Just a normal reply about files."
		if got := strings.TrimSpace(StripNonspokenBlocks(raw)); got != raw {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_emoji_removed", func(t *testing.T) {
		spoken := spokenDefault("Done! \U0001F389\U0001F680 All tests pass ✅")
		assertSpokenOmits(t, spoken, "\U0001F389", "\U0001F680", "✅")
		assertSpokenContains(t, spoken, "All tests pass")
	})
	t.Run("test_no_newlines_in_output", func(t *testing.T) {
		spoken := spokenDefault("First line\nSecond line\n\nThird paragraph")
		assertSpokenOmits(t, spoken, "\n")
		assertSpokenContains(t, spoken, "First line", "Third paragraph")
	})
	t.Run("test_existing_punctuation_not_doubled", func(t *testing.T) {
		out := FlattenNewlinesForPayload("Alpha.\nBeta!")
		assertSpokenOmits(t, out, "..")
		assertSpokenContains(t, out, "Alpha.", "Beta!")
	})
	t.Run("test_tool_path_strips_think_blocks", func(t *testing.T) {
		cleaned := PrepareSpokenTextUncapped("<think>hidden</think>**Loud** and clear \U0001F389")
		assertSpokenOmits(t, cleaned, "hidden", "**", "\U0001F389")
		assertSpokenContains(t, cleaned, "Loud and clear")
	})
	t.Run("test_streaming_helper_uses_shared_cleaner", func(t *testing.T) {
		cleaned := PrepareSpokenTextUncapped("Temp is 14°C today\nand rising")
		assertSpokenContains(t, cleaned, "degrees Celsius")
		assertSpokenOmits(t, cleaned, "\n")
	})
}

// Code only replies clean to nothing and a rich reply keeps its words without markup
func TestTTSMarkdownStripping(t *testing.T) {
	t.Run("test_empty_after_stripping_returns_empty", func(t *testing.T) {
		if got := PrepareSpokenTextUncapped("```python\nprint('hello')\n```"); got != "" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("test_complex_response", func(t *testing.T) {
		text := "## Answer\n\nHere's how to do it:\n\n```python\ndef hello():\n    print('hi')\n```\n\nRun it with `python main.py`. See [docs](https://example.com) for more.\n\n- Step one\n- Step two\n\n---\n\n**Good luck!**"
		result := PrepareSpokenTextUncapped(text)
		assertSpokenOmits(t, result, "```", "https://", "**", "---")
		assertSpokenContains(t, result, "Answer", "Good luck!", "docs")
	})
	t.Run("cap_counts_code_points_and_rstrips", func(t *testing.T) {
		n := 7
		if got := PrepareSpokenText("Café au lait tonight", &n); got != "Café au" {
			t.Fatalf("got %q", got)
		}
		zero := 0
		if got := PrepareSpokenText("Short reply", &zero); got != "Short reply" {
			t.Fatalf("zero cap %q", got)
		}
	})
}
