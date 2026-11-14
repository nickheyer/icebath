package codeexec

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

const helperProbe = `
from __future__ import annotations
%s
import json
print(json.dumps([json_parse('{"value": 7}')["value"],
                  shell_quote("two words"), retry(lambda: "ok", delay=0)]))
`

// Dispatches execute_code through the registered definition with an empty enabled tool list
func dispatchExecuteCode(t *testing.T, env *tools.Env, code string, reset bool) map[string]any {
	t.Helper()
	def := tools.Default.Get("execute_code")
	if def == nil {
		t.Fatal("execute_code is not registered")
	}
	ctx := tools.WithEnabledTools(context.Background(), []string{})
	call := &tools.Call{Name: "execute_code", Args: map[string]any{"code": code, "reset": reset}, TaskID: "helper-contract", Env: env}
	return decodeResult(t, def.Handler(ctx, call))
}

func TestSchemaHelperInstructionsWorkAcrossKernelLifetimes(t *testing.T) {
	env := kernelEnv(t, nil)
	description, _ := tools.Default.Get("execute_code").Schema["description"].(string)
	var imports []string
	for _, m := range regexp.MustCompile("`(from icebath_tools import [\\w, ]+)`").FindAllStringSubmatch(description, -1) {
		imports = append(imports, m[1])
	}
	probe := strings.Replace(helperProbe, "%s", strings.Join(imports, "\n"), 1)
	for _, step := range []struct{ reset, reused bool }{{false, false}, {false, true}, {true, false}} {
		result := dispatchExecuteCode(t, env, probe, step.reset)
		wantStatus(t, result, "success")
		var got []any
		if err := json.Unmarshal([]byte(outputOf(result)), &got); err != nil || len(got) != 3 || got[0] != 7.0 || got[1] != "'two words'" || got[2] != "ok" {
			t.Fatalf("helpers misbehaved: %v", result)
		}
		if kernelField(result, "reused") != step.reused {
			t.Fatalf("reused %v, want %v: %v", kernelField(result, "reused"), step.reused, result)
		}
	}
}

func TestMissingHelperRecoveryInstructionsExecute(t *testing.T) {
	env := kernelEnv(t, nil)
	pattern := regexp.MustCompile(`from icebath_tools import \w+`)
	for _, helper := range []string{"json_parse", "shell_quote", "retry"} {
		failed := dispatchExecuteCode(t, env, "print("+helper+")", true)
		wantStatus(t, failed, "error")
		hint, _ := failed["hint"].(string)
		instruction := pattern.FindString(hint)
		if instruction == "" {
			t.Fatalf("no import instruction in the hint: %v", failed)
		}
		recovered := dispatchExecuteCode(t, env, instruction+"\nprint(callable("+helper+"))", false)
		wantStatus(t, recovered, "success")
		if strings.TrimSpace(outputOf(recovered)) != "True" {
			t.Fatalf("recovery did not import the helper: %v", recovered)
		}
		skew := sandboxFailureHint("ImportError: cannot import name '"+helper+"' from 'icebath_tools'", nil)
		if !strings.Contains(skew, instruction) {
			t.Fatalf("skew hint %q lacks %q", skew, instruction)
		}
	}
}
