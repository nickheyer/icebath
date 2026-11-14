package tools

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func addressSchema(t *testing.T) any {
	t.Helper()
	schema, err := pyjson.Loads(`{"type": "object", "properties": {"city": {"type": "string"}, "zip": {"type": "string"}}, "required": ["city"]}`)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

// test_delegate_output_schema.py TestValidateOutput
func TestValidateOutput(t *testing.T) {
	schema := addressSchema(t)
	check := func(t *testing.T, text string, wantOK bool, mention string) {
		t.Helper()
		ok, errs, err := ValidateOutput(text, schema)
		if err != nil {
			t.Fatal(err)
		}
		if ok != wantOK || (ok && len(errs) != 0) || (!ok && len(errs) == 0) {
			t.Fatalf("ok %v errors %q", ok, errs)
		}
		if mention != "" && !strings.Contains(strings.Join(errs, "\n"), mention) {
			t.Fatalf("errors %q miss %q", errs, mention)
		}
	}
	t.Run("test_valid_json_matching_schema", func(t *testing.T) { check(t, `{"city": "Berlin"}`, true, "") })
	t.Run("test_json_violating_schema_reports_errors", func(t *testing.T) { check(t, `{"zip": "10115"}`, false, "city") })
	t.Run("test_non_json_text_reports_parse_error", func(t *testing.T) { check(t, "I could not produce JSON, sorry.", false, "") })
	t.Run("test_code_fenced_json_is_accepted", func(t *testing.T) { check(t, "```json\n{\"city\": \"Oslo\"}\n```", true, "") })
	t.Run("test_json_embedded_in_prose_is_extracted", func(t *testing.T) {
		check(t, "Here is the result:\n{\"city\": \"Lima\"}\nHope that helps!", true, "")
	})
	t.Run("test_empty_text_is_invalid", func(t *testing.T) { check(t, "", false, "") })
	_, errs, _ := ValidateOutput(`{"zip": 5, "tags": [1]}`, schema)
	if strings.Join(errs, "|") != "$: 'city' is a required property|$.zip: 5 is not of type 'string'" {
		t.Fatalf("rendered errors %q", errs)
	}
	if ok, errs, _ := ValidateOutput("```json\n[{\"city\": \"A\"}, {\"city\": \"B\"}]\n```", map[string]any{"type": "array"}); !ok {
		t.Fatalf("fenced array rejected %q", errs)
	}
}

// test_delegate_output_schema.py TestPromptPlumbing
func TestPromptPlumbing(t *testing.T) {
	t.Run("test_contract_block_carries_schema", func(t *testing.T) {
		out := AppendOutputContract("base context", addressSchema(t))
		if !strings.Contains(out, "base context") || !strings.Contains(out, "OUTPUT CONTRACT") || !strings.Contains(out, `"city"`) {
			t.Fatalf("contract %q", out)
		}
		if AppendOutputContract("  ", map[string]any{})[:15] != "OUTPUT CONTRACT" {
			t.Fatal("blank context keeps a leading separator")
		}
	})
	t.Run("test_retry_message_carries_verbatim_errors", func(t *testing.T) {
		msg := BuildRetryMessage([]string{"'city' is a required property"})
		if !strings.Contains(msg, "'city' is a required property") || !strings.Contains(msg, "JSON") {
			t.Fatalf("retry %q", msg)
		}
	})
}

// test_delegate_output_schema.py TestToolSchemaSurface test_output_schema_advertised_per_task_only
func TestOutputSchemaAdvertisedPerTaskOnly(t *testing.T) {
	props := DelegateTaskSchema["parameters"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["output_schema"]; ok {
		t.Fatal("legacy top level output_schema advertised")
	}
	taskProps := props["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if taskProps["output_schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("task output_schema %v", taskProps["output_schema"])
	}
}

// test_delegate_output_schema.py test_non_dict_output_schema_rejected and test_invalid_json_schema_rejected_at_dispatch coercion half
func TestCoerceOutputSchema(t *testing.T) {
	for _, tc := range []struct {
		raw    any
		prefix string
	}{
		{"not-a-dict", "output_schema must be a JSON Schema object, got a non-JSON string."},
		{"[1]", "output_schema must be a JSON Schema object."},
		{int64(3), "output_schema must be a JSON Schema object, got int."},
		{map[string]any{"type": int64(42)}, "output_schema is not a valid JSON Schema: 42 is not valid under any of the given schemas\n\nFailed validating 'anyOf'"},
	} {
		schema, msg := CoerceOutputSchema(tc.raw)
		if schema != nil || !strings.HasPrefix(msg, tc.prefix) {
			t.Fatalf("%v: %v %q", tc.raw, schema, msg)
		}
	}
	if schema, msg := CoerceOutputSchema(nil); schema != nil || msg != "" {
		t.Fatal("absent schema not passed through")
	}
	schema, msg := CoerceOutputSchema(`{"type": "object"}`)
	if msg != "" || !isJSONObject(schema) {
		t.Fatalf("double encoded schema %v %q", schema, msg)
	}
}

const delegationSecret = "sk-ant-syntheticdelegationcredentialsyntheticdelegationcredentialsyntheticdelegationcredential"

// test_delegated_output_redaction.py test_structured_summary_is_redacted_per_value_and_stays_valid_json
func TestStructuredSummaryIsRedactedPerValueAndStaysValidJSON(t *testing.T) {
	key := "sk-proj-" + strings.Repeat("a", 40)
	answer := `{"env": "OPENAI_API_KEY=` + key + `", "n": 2}`
	for name, wrap := range map[string]func(string) string{
		"bare":   func(s string) string { return s },
		"fenced": func(s string) string { return "```json\n" + s + "\n```" },
		"prose":  func(s string) string { return "Here is the answer:\n" + s + "\nDone." },
	} {
		t.Run(name, func(t *testing.T) {
			entry := RedactChildResult(nil, map[string]any{"summary": wrap(answer), "schema_valid": true}, map[string]any{"type": "object"}, true).(map[string]any)
			parsed, err := pyjson.Loads(ExtractJSONCandidate(entry["summary"].(string)))
			if err != nil {
				t.Fatalf("summary %q no longer JSON: %v", entry["summary"], err)
			}
			obj := parsed.(*pyjson.Object)
			if obj.Vals["n"] != int64(2) || strings.Contains(obj.Vals["env"].(string), key) || entry["schema_valid"] != true {
				t.Fatalf("entry %v", entry)
			}
		})
	}
}

// test_delegated_output_redaction.py test_redacted_schema_verdict_describes_delivered_summary_without_rerunning_child projection half
func TestRedactedSchemaVerdictDescribesDeliveredSummary(t *testing.T) {
	schema, _ := pyjson.Loads(`{"type": "object", "properties": {"value": {"const": "` + delegationSecret + `"}}, "required": ["value"]}`)
	entry := RedactChildResult(nil, map[string]any{"status": "completed", "summary": `{"value": "` + delegationSecret + `"}`, "schema_valid": true}, schema, true).(map[string]any)
	text, _ := pyjson.Dumps(entry)
	if entry["status"] != "completed" || entry["schema_valid"] != false || strings.Contains(text, delegationSecret) {
		t.Fatalf("entry %s", text)
	}
	if entry["schema_note"] != "Secret redaction changed the delivered summary; do not treat it as validated output." {
		t.Fatalf("note %v", entry["schema_note"])
	}
	stale := RedactChildResult(nil, map[string]any{"summary": delegationSecret, "schema_valid": true, "schema_errors": []any{"old"}}, nil, true).(map[string]any)
	if errs := stale["schema_errors"].([]any); stale["schema_valid"] != false || len(errs) != 1 || errs[0] != "The redacted summary has not been validated against the original output_schema." {
		t.Fatalf("durable entry without schema %v", stale)
	}
}

// test_delegated_output_redaction.py test_structured_scalar_summary_stays_valid_json projection half
func TestStructuredScalarSummaryStaysValidJSON(t *testing.T) {
	for _, tc := range []struct{ summary, schema string }{
		{`"OPENAI_API_KEY=opaqueSyntheticValueXYZ1234"`, `{"type": "string"}`},
		{`"ordinary scalar answer"`, `{"type": "string"}`},
		{`null`, `{"type": "null"}`},
		{`true`, `{"type": "boolean"}`},
		{`17`, `{"type": "integer"}`},
		{`1.5`, `{"type": "number"}`},
	} {
		schema, _ := pyjson.Loads(tc.schema)
		entry := RedactChildResult(nil, map[string]any{"summary": tc.summary, "schema_valid": true}, schema, true).(map[string]any)
		summary := entry["summary"].(string)
		delivered, err := pyjson.Loads(summary)
		if err != nil || entry["schema_valid"] != true {
			t.Fatalf("%s: summary %q valid %v err %v", tc.summary, summary, entry["schema_valid"], err)
		}
		if strings.HasPrefix(tc.summary, `"OPENAI_API_KEY=`) {
			if s := delivered.(string); strings.Contains(s, "opaqueSyntheticValueXYZ1234") || !strings.HasPrefix(s, "OPENAI_API_KEY=") {
				t.Fatalf("scalar secret %q", s)
			}
		} else if summary != tc.summary {
			t.Fatalf("unchanged summary rewritten %q", summary)
		}
	}
}

// test_delegated_output_redaction.py test_parent_copy_honours_opt_out_while_persisted_copies_stay_forced projection half
func TestParentCopyHonoursOptOutWhilePersistedCopiesStayForced(t *testing.T) {
	env := &Env{Secrets: config.NewSecrets(t.TempDir())}
	if err := env.Secrets.Set("ICEBATH_REDACT_SECRETS", "false"); err != nil {
		t.Fatal(err)
	}
	if got := RedactChildText(env, delegationSecret, false); got != delegationSecret {
		t.Fatalf("opt out parent copy %q", got)
	}
	forced := RedactChildText(env, delegationSecret, true)
	if strings.Contains(forced, delegationSecret) {
		t.Fatalf("forced copy %q", forced)
	}
	carrier := NewParentCopy(delegationSecret, forced)
	if RedactChildText(env, carrier, false) != delegationSecret || RedactChildText(env, carrier, true) != forced {
		t.Fatal("parent copy projections")
	}
	if NewParentCopy("same", "same") != "same" {
		t.Fatal("equal copies kept a carrier")
	}
	event := RedactCompletionEvent(env, map[string]any{"goal": delegationSecret, "summary": carrier, "status": "completed"}, true).(map[string]any)
	if event["goal"] != delegationSecret || event["summary"] != forced || event["status"] != "completed" {
		t.Fatalf("event %v", event)
	}
	text, _ := pyjson.Dumps(map[string]any{"summary": carrier})
	if !strings.Contains(text, delegationSecret) {
		t.Fatalf("carrier serialized %s", text)
	}
	if RedactChildValue(env, struct{}{}, true) != RedactionUnavailable {
		t.Fatal("unsupported value not withheld")
	}
}
