package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Largest framed line either side of the managed worker pipe carries
const MaxManagedFrame = 4 * 1024 * 1024

// Bounded run_conversation fields a managed turn receipt keeps
var managedResultFields = []string{"final_response", "failed", "interrupted", "completed", "partial", "error", "turn_exit_reason",
	"api_calls", "model", "provider", "prompt_tokens", "completion_tokens", "total_tokens",
	"cache_read_tokens", "cache_write_tokens", "reasoning_tokens",
	"estimated_cost_usd", "cost_status", "cost_source", "service_tier", "failure_reason",
	"session_id", "requested_model", "served_model", "_turn_input_tokens", "_turn_output_tokens",
	"failure_retryable", "failure_resets_at", "billing_block"}

// Structured result fields and the keys each may carry
var managedStructuredFields = map[string][]string{
	"billing_block": {"provider", "provider_label", "model", "billing_url", "message", "unverified"},
}

// Ordered names of the structured result fields
var managedStructuredOrder = []string{"billing_block"}

// Whether a value is None, a bool, int or float, or a string of at most 4096 characters
func managedScalar(value any) bool {
	switch typed := value.(type) {
	case nil, bool, int, int64, float64:
		return true
	case string:
		return utf8.RuneCountInString(typed) <= 4096
	}
	return false
}

// Whether a value is a dict of allowed scalar keys of a structured field
func managedStructured(key string, value any) bool {
	block, isDict := value.(*jsonutil.Object)
	if !isDict || block == nil {
		return false
	}
	for _, name := range block.Keys() {
		if !slices.Contains(managedStructuredFields[key], name) || !managedScalar(block.Value(name)) {
			return false
		}
	}
	return true
}

// Whether a value is None or a bool, int, float or string
func managedWireValue(value any) bool {
	switch value.(type) {
	case nil, bool, int, int64, float64, string:
		return true
	}
	return false
}

// Worker side receipt fields of a run_conversation result, scalars only and text bounded
func ResultFrame(result *jsonutil.Object) *jsonutil.Object {
	frame := jsonutil.NewObject()
	for _, key := range result.Keys() {
		value := result.Value(key)
		if !slices.Contains(managedResultFields, key) || managedStructuredFields[key] != nil || !managedWireValue(value) {
			continue
		}
		if text, isText := value.(string); isText && key != "final_response" {
			value = textutil.First(text, 4096)
		}
		frame.Set(key, value)
	}
	for _, key := range managedStructuredOrder {
		block, isDict := result.ValueOr(key, nil).(*jsonutil.Object)
		if !isDict || block == nil {
			continue
		}
		kept := jsonutil.NewObject()
		for _, name := range block.Keys() {
			value := block.Value(name)
			if !slices.Contains(managedStructuredFields[key], name) || !managedWireValue(value) {
				continue
			}
			if text, isText := value.(string); isText {
				value = textutil.First(text, 4096)
			}
			kept.Set(name, value)
		}
		frame.Set(key, kept)
	}
	return frame
}

// Invalid worker result refusal
func invalidWorkerResult() error {
	return &PyError{Kind: "ValueError", Msg: "invalid_worker_result"}
}

// Sum of two receipt scalars: texts joined, numbers added with bools as 0 and 1, a TypeError for a mixed text and number pair
func scalarAdd(left, right any) (any, error) {
	if l, isText := left.(string); isText {
		if r, both := right.(string); both {
			return l + r, nil
		}
	}
	_, leftText := left.(string)
	_, rightText := right.(string)
	if leftText || rightText || left == nil || right == nil {
		return nil, &PyError{Kind: "TypeError", Msg: fmt.Sprintf("cannot add %s to %s", errchain.Kind(right), errchain.Kind(left))}
	}
	lf, leftFloat := left.(float64)
	rf, rightFloat := right.(float64)
	if leftFloat || rightFloat {
		if !leftFloat {
			lf = scalarFloat(left)
		}
		if !rightFloat {
			rf = scalarFloat(right)
		}
		return lf + rf, nil
	}
	return scalarInt(left) + scalarInt(right), nil
}

// Integer of an int or bool scalar, a bool counting as 0 or 1
func scalarInt(value any) int64 {
	if flag, isBool := value.(bool); isBool {
		if flag {
			return 1
		}
		return 0
	}
	n, _ := anyutil.Int(value)
	return n
}

// Float of an int or bool scalar
func scalarFloat(value any) float64 { return float64(scalarInt(value)) }

// Value or zero when falsy, the Python value or 0
func orZero(value any) any {
	if !anyutil.IsTrue(value) {
		return int64(0)
	}
	return value
}

// Owner side of the worker result frame, the in process turn shaped result and usage or a ValueError
func AcceptResult(result any) (*jsonutil.Object, *jsonutil.Object, error) {
	frame, isDict := result.(*jsonutil.Object)
	if !isDict || frame == nil {
		return nil, nil, invalidWorkerResult()
	}
	for _, key := range frame.Keys() {
		if !slices.Contains(managedResultFields, key) {
			return nil, nil, invalidWorkerResult()
		}
	}
	if _, isText := frame.ValueOr("final_response", nil).(string); !isText {
		return nil, nil, invalidWorkerResult()
	}
	for _, key := range managedStructuredOrder {
		if value := frame.ValueOr(key, nil); value != nil && !managedStructured(key, value) {
			return nil, nil, invalidWorkerResult()
		}
	}
	for _, key := range frame.Keys() {
		if key != "final_response" && managedStructuredFields[key] == nil && !managedScalar(frame.Value(key)) {
			return nil, nil, invalidWorkerResult()
		}
	}
	accepted := frame.Clone()
	for _, key := range managedStructuredOrder {
		if accepted.ValueOr(key, nil) == nil {
			accepted.Take(key)
		}
	}
	turnIn, _ := accepted.Take("_turn_input_tokens")
	turnOut, _ := accepted.Take("_turn_output_tokens")
	for _, value := range []any{turnIn, turnOut} {
		if value == nil {
			continue
		}
		n, isInt := anyutil.Int(value)
		if _, isBool := value.(bool); isBool || !isInt || n < 0 {
			return nil, nil, invalidWorkerResult()
		}
	}
	incoming, _ := accepted.Take("prompt_tokens")
	outgoing, _ := accepted.Take("completion_tokens")
	if incoming == nil && outgoing == nil {
		return accepted, jsonutil.NewObject(), nil
	}
	input, output := turnIn, turnOut
	if turnIn == nil {
		input = orZero(incoming)
	}
	if turnOut == nil {
		output = orZero(outgoing)
	}
	total, err := scalarAdd(input, output)
	if err != nil {
		return nil, nil, err
	}
	usage := jsonutil.ObjectOf("input_tokens", input, "output_tokens", output, "total_tokens", total)
	accepted.Set("input_tokens", orZero(incoming))
	accepted.Set("output_tokens", orZero(outgoing))
	return accepted, usage, nil
}

// One framed line of the pipe, EOFError at end, ValueError for an oversized, unterminated or non dict frame
func ReadFrame(stream *bufio.Reader) (*jsonutil.Object, error) {
	var line []byte
	for len(line) <= MaxManagedFrame {
		chunk, err := stream.ReadSlice('\n')
		line = append(line, chunk...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		return nil, &PyError{Kind: "OSError", Msg: err.Error()}
	}
	if len(line) == 0 {
		return nil, &PyError{Kind: "EOFError", Msg: "managed_worker_pipe_closed"}
	}
	if len(line) > MaxManagedFrame || line[len(line)-1] != '\n' {
		return nil, &PyError{Kind: "ValueError", Msg: "managed_worker_frame_too_large"}
	}
	decoded, err := jsonutil.Decode(line)
	if err != nil {
		return nil, &PyError{Kind: "JSONDecodeError", Msg: err.Error()}
	}
	frame, isDict := decoded.(*jsonutil.Object)
	if !isDict {
		return nil, &PyError{Kind: "ValueError", Msg: "invalid_managed_worker_frame"}
	}
	return frame, nil
}

// Compact ASCII JSON line of a frame, ValueError past the frame ceiling or for a non finite float
func EncodeFrame(frame any) ([]byte, error) {
	text, err := jsonutil.Encode(frame, jsonutil.ASCII())
	if err != nil {
		var typeErr *json.UnsupportedTypeError
		if errors.As(err, &typeErr) {
			return nil, &PyError{Kind: "TypeError", Msg: err.Error()}
		}
		return nil, &PyError{Kind: "ValueError", Msg: err.Error()}
	}
	data := append([]byte(text), '\n')
	if len(data) > MaxManagedFrame {
		return nil, &PyError{Kind: "ValueError", Msg: "managed_worker_frame_too_large"}
	}
	return data, nil
}
