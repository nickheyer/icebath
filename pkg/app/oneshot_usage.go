package app

import (
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Exit code of a turn stopped by an interrupt, the SIGINT convention
const oneshotInterruptedExitCode = 130

// Run result keys copied into the usage file report
var oneshotUsageKeys = []string{"estimated_cost_usd", "cost_status", "cost_source", "input_tokens", "output_tokens",
	"cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "total_tokens", "api_calls",
	"model", "provider", "session_id", "completed", "partial", "interrupted", "turn_exit_reason"}

// Counters summed per auxiliary task into the auxiliary block of the report
var oneshotAuxCounters = agent.OneshotAuxCounters

// Exit code of a finished one shot turn, 130 interrupted, 2 failed or partial, 1 without text, else 0
func oneshotExitCode(response string, result map[string]any) int {
	if anyutil.IsTrue(result["interrupted"]) {
		return oneshotInterruptedExitCode
	}
	if completed, ok := result["completed"]; anyutil.IsTrue(result["failed"]) || anyutil.IsTrue(result["partial"]) || (ok && completed == false) {
		return 2
	}
	if strings.TrimSpace(response) == "" {
		return 1
	}
	return 0
}

// Python addition of two JSON numbers, ints staying exact and a float operand making a float
func pyNumAdd(a, b any) any {
	toBig := func(v any) (*big.Int, bool) {
		switch n := v.(type) {
		case int64:
			return big.NewInt(n), true
		case int:
			return big.NewInt(int64(n)), true
		case bool:
			if n {
				return big.NewInt(1), true
			}
			return big.NewInt(0), true
		case *big.Int:
			return n, true
		}
		return nil, false
	}
	toFloat := func(v any) float64 {
		if f, ok := v.(float64); ok {
			return f
		}
		if n, ok := toBig(v); ok {
			f, _ := new(big.Float).SetInt(n).Float64()
			return f
		}
		panic(fmt.Sprintf("cannot add %s and %s as usage counts", errchain.Kind(a), errchain.Kind(b)))
	}
	x, okA := toBig(a)
	y, okB := toBig(b)
	if okA && okB {
		sum := new(big.Int).Add(x, y)
		if sum.IsInt64() {
			return sum.Int64()
		}
		return sum
	}
	return toFloat(a) + toFloat(b)
}

// The value when truthy, otherwise the integer zero
func orIntZero(v any) any {
	if anyutil.IsTrue(v) {
		return v
	}
	return int64(0)
}

// Adds the auxiliary breakdown and the total including auxiliary usage to the ledger
func oneshotAuxiliaryReport(report *jsonutil.Object, byTask *jsonutil.Object) {
	totals := jsonutil.NewObject()
	for _, key := range oneshotAuxCounters {
		var sum any = int64(0)
		for _, task := range byTask.Keys() {
			counters, _ := jsonutil.AsObject(byTask.Value(task))
			var value any
			if counters != nil {
				value = counters.Value(key)
			}
			sum = pyNumAdd(sum, orIntZero(value))
		}
		totals.Set(key, sum)
	}
	totals.Set("total_tokens", pyNumAdd(totals.Value("input_tokens"), totals.Value("output_tokens")))
	auxiliary := jsonutil.NewObject()
	for _, key := range totals.Keys() {
		auxiliary.Set(key, totals.Value(key))
	}
	auxiliary.Set("by_task", byTask)
	report.Set("auxiliary", auxiliary)
	mainCost := report.Value("estimated_cost_usd")
	var cost any
	if mainCost != nil {
		cost = pyNumAdd(mainCost, totals.Value("estimated_cost_usd"))
	}
	report.Set("total_including_auxiliary", jsonutil.ObjectOf(
		"estimated_cost_usd", cost,
		"total_tokens", pyNumAdd(orIntZero(report.Value("total_tokens")), totals.Value("total_tokens")),
		"api_calls", pyNumAdd(orIntZero(report.Value("api_calls")), totals.Value("api_calls"))))
}

// Writes the usage file JSON report for pipelines, swallowing every failure so the run outcome stands
func oneshotWriteUsageFile(path string, result *jsonutil.Object, failure any) {
	if path == "" {
		return
	}
	func() {
		defer func() { _ = recover() }()
		report := jsonutil.NewObject()
		for _, key := range oneshotUsageKeys {
			report.Set(key, result.Value(key))
		}
		report.Set("failed", anyutil.IsTrue(result.Value("failed")) || failure != nil)
		report.Set("service_tier", result.Value("service_tier"))
		if byTask, ok := result.Value("auxiliary_usage").(*jsonutil.Object); ok {
			oneshotAuxiliaryReport(report, byTask)
		}
		if failure != nil {
			report.Set("failure", failure)
		}
		out := pathutil.ExpandUser(path)
		if err := os.MkdirAll(filepath.Dir(out), 0o777); err != nil {
			return
		}
		text, err := jsonutil.Encode(report, jsonutil.Indent(2), jsonutil.ASCII())
		if err != nil {
			return
		}
		_ = os.WriteFile(out, []byte(text+"\n"), 0o666)
	}()
}
