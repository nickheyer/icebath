package tool

import (
	"encoding/json"
	"math"
	"strconv"
	"sync"
	"time"
)

const (
	ProgressEvent       = "tool.progress"
	progressMinInterval = 500 * time.Millisecond
)

type ProgressRelay struct {
	mu   sync.Mutex
	last time.Time
	call *Call
}

func NewProgressRelay(c *Call) *ProgressRelay {
	if c == nil || c.Progress == nil {
		return nil
	}
	return &ProgressRelay{call: c}
}

func (p *ProgressRelay) Report(progress float64, total *float64, message string) {
	if p == nil {
		return
	}
	done := total != nil && *total > 0 && progress >= *total
	now := time.Now()
	p.mu.Lock()
	if !p.last.IsZero() && !done && now.Sub(p.last) < progressMinInterval {
		p.mu.Unlock()
		return
	}
	p.last = now
	p.mu.Unlock()
	fields := map[string]any{"tool_call_id": p.call.ID, "progress": progress, "total": nil, "percent": nil}
	if total != nil {
		fields["total"] = *total
		if *total != 0 {
			fields["percent"] = int(math.RoundToEven(math.Min(math.Max(progress / *total, 0), 1) * 100))
		}
	}
	p.call.Progress.Progress(p.call, message, fields)
}

func ProgressPayload(name, message string, fields map[string]any) json.RawMessage {
	id, _ := fields["tool_call_id"].(string)
	progress, _ := fields["progress"].(float64)
	d := newObj().with("tool_id", newStr(id)).with("name", newStr(name)).
		with("progress", newNum(floatValue(progress)))
	switch t := fields["total"].(type) {
	case float64:
		d.set("total", newNum(floatValue(t)))
	default:
		d.set("total", &jnode{kind: jNull})
	}
	switch t := fields["percent"].(type) {
	case int:
		d.set("percent", newNum(strconv.Itoa(t)))
	default:
		d.set("percent", &jnode{kind: jNull})
	}
	if message != "" {
		d.set("message", newStr(message))
	} else {
		d.set("message", &jnode{kind: jNull})
	}
	return d.compact()
}

func floatValue(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e16 {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	return floatText(f)
}
