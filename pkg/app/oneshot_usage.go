package app

import (
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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
	if pyrepr.Truthy(result["interrupted"]) {
		return oneshotInterruptedExitCode
	}
	if completed, ok := result["completed"]; pyrepr.Truthy(result["failed"]) || pyrepr.Truthy(result["partial"]) || (ok && completed == false) {
		return 2
	}
	if pystr.Strip(response) == "" {
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
		panic(fmt.Sprintf("TypeError: unsupported operand type(s) for +: '%s' and '%s'", pyrepr.TypeName(a), pyrepr.TypeName(b)))
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
	if pyrepr.Truthy(v) {
		return v
	}
	return int64(0)
}

// Adds the auxiliary breakdown and the total including auxiliary usage to the ledger
func oneshotAuxiliaryReport(report *pyjson.Object, byTask *pyjson.Object) {
	totals := pyjson.NewObject()
	for _, key := range oneshotAuxCounters {
		var sum any = int64(0)
		for _, task := range byTask.Keys {
			counters, _ := pyjson.AsObject(byTask.Vals[task])
			var value any
			if counters != nil {
				value = counters.Vals[key]
			}
			sum = pyNumAdd(sum, orIntZero(value))
		}
		totals.Set(key, sum)
	}
	totals.Set("total_tokens", pyNumAdd(totals.Vals["input_tokens"], totals.Vals["output_tokens"]))
	auxiliary := pyjson.NewObject()
	for _, key := range totals.Keys {
		auxiliary.Set(key, totals.Vals[key])
	}
	auxiliary.Set("by_task", byTask)
	report.Set("auxiliary", auxiliary)
	mainCost := report.Vals["estimated_cost_usd"]
	var cost any
	if mainCost != nil {
		cost = pyNumAdd(mainCost, totals.Vals["estimated_cost_usd"])
	}
	report.Set("total_including_auxiliary", pyjson.ObjectOf(
		"estimated_cost_usd", cost,
		"total_tokens", pyNumAdd(orIntZero(report.Vals["total_tokens"]), totals.Vals["total_tokens"]),
		"api_calls", pyNumAdd(orIntZero(report.Vals["api_calls"]), totals.Vals["api_calls"])))
}

// Writes the usage file JSON report for pipelines, swallowing every failure so the run outcome stands
func oneshotWriteUsageFile(path string, result *pyjson.Object, failure any) {
	if path == "" {
		return
	}
	func() {
		defer func() { _ = recover() }()
		report := pyjson.NewObject()
		for _, key := range oneshotUsageKeys {
			report.Set(key, result.Vals[key])
		}
		report.Set("failed", pyrepr.Truthy(result.Vals["failed"]) || failure != nil)
		report.Set("service_tier", result.Vals["service_tier"])
		if byTask, ok := result.Vals["auxiliary_usage"].(*pyjson.Object); ok {
			oneshotAuxiliaryReport(report, byTask)
		}
		if failure != nil {
			report.Set("failure", failure)
		}
		out := pypath.Expanduser(path)
		if err := os.MkdirAll(filepath.Dir(out), 0o777); err != nil {
			return
		}
		text, err := pyjson.Dumps(report, pyjson.Indent(2))
		if err != nil {
			return
		}
		_ = os.WriteFile(out, []byte(text+"\n"), 0o666)
	}()
}
