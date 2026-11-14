package local

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	textStageShare  = 0.85
	reconnectDelay  = 3 * time.Second
	staleEntryTTL   = 120 * time.Second
	sseIdleTimeout  = 60 * time.Second
	prefillTimeout  = 2 * time.Second
	textModelStage  = "text_model"
	statusLoading   = "loading"
	eventModelRemov = "model_remove"
)

var loadEvents = map[string]bool{"status_change": true, "model_status": true}

type loadEntry struct {
	stage   string
	value   float64
	percent int
	ts      time.Time
}

type LoadProgress struct {
	Stage   string
	Value   float64
	Percent int
}

func percentOf(x float64) int { return max(0, min(100, int(math.RoundToEven(x*100)))) }

func compositePercent(stages []string, current string, value float64) int {
	found := false
	for _, s := range stages {
		if s == current {
			found = true
		}
	}
	if len(stages) <= 1 || !found {
		return percentOf(value)
	}
	extras := 0
	for _, s := range stages {
		if s != textModelStage {
			extras++
		}
	}
	extraShare := 0.0
	if extras > 0 {
		extraShare = (1.0 - textStageShare) / float64(extras)
	}
	offset := 0.0
	for _, stage := range stages {
		share := extraShare
		if stage == textModelStage {
			share = textStageShare
		}
		if stage == current {
			return percentOf(offset + share*value)
		}
		offset += share
	}
	return percentOf(value)
}

func (r *Runtime) applyLoadEvent(model, event string, data map[string]any) {
	r.progMu.Lock()
	defer r.progMu.Unlock()
	status := jsonText(data["status"])
	switch {
	case loadEvents[event] && status == statusLoading:
		progress, _ := data["progress"].(map[string]any)
		var stages []string
		if list, ok := progress["stages"].([]any); ok {
			for _, s := range list {
				stages = append(stages, jsonText(s))
			}
		}
		current := jsonText(progress["current"])
		entry, ok := r.loading[model]
		if !ok {
			entry = &loadEntry{}
			r.loading[model] = entry
		}
		entry.ts = time.Now()
		value, isNumber := progress["value"].(float64)
		if b, isBool := progress["value"].(bool); isBool {
			value, isNumber = 0, true
			if b {
				value = 1
			}
		}
		if current != "" && isNumber {
			entry.stage, entry.value, entry.percent = current, value, compositePercent(stages, current, value)
		}
	case (loadEvents[event] || event == eventModelRemov) && status != statusLoading:
		delete(r.loading, model)
	}
}

func (r *Runtime) clearLoading() {
	r.progMu.Lock()
	clear(r.loading)
	r.progMu.Unlock()
}

func (r *Runtime) streamLoads(ctx context.Context, base, apiKey string) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, base+"/models/sse", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "text/event-stream")
	idle := time.AfterFunc(sseIdleTimeout, cancel)
	defer idle.Stop()
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := httpclient.CheckStatus(resp); err != nil {
		return err
	}
	reader := bufio.NewReader(resp.Body)
	for {
		idle.Reset(sseIdleTimeout)
		line, err := reader.ReadString('\n')
		if err != nil {
			if streamCtx.Err() != nil && ctx.Err() == nil {
				return fmt.Errorf("no event for %s", sseIdleTimeout)
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		text := strings.TrimSpace(strings.ToValidUTF8(line, "�"))
		if !strings.HasPrefix(text, "data:") {
			continue
		}
		var msg map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(text[5:])), &msg) != nil {
			continue
		}
		data, _ := msg["data"].(map[string]any)
		r.applyLoadEvent(jsonText(msg["model"]), jsonText(msg["event"]), data)
	}
}

func (r *Runtime) watchLoads(ctx context.Context) {
	for {
		if base, key, ok := r.managedRoot(); ok {
			if err := r.streamLoads(ctx, base, key); err != nil && ctx.Err() == nil {
				slog.Debug(fmt.Sprintf("load-progress SSE reconnecting: %v", err))
			}
		}
		r.clearLoading()
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

func (r *Runtime) ensureLoadWatcher() {
	r.progMu.Lock()
	if r.watching {
		r.progMu.Unlock()
		return
	}
	r.watching = true
	r.progMu.Unlock()
	r.goBackground(func(ctx context.Context) {
		defer func() {
			r.progMu.Lock()
			r.watching = false
			r.progMu.Unlock()
		}()
		r.watchLoads(ctx)
	})
}

func (r *Runtime) LoadingProgress() map[string]LoadProgress {
	r.ensureLoadWatcher()
	now := time.Now()
	r.progMu.Lock()
	defer r.progMu.Unlock()
	out := map[string]LoadProgress{}
	for model, e := range r.loading {
		if now.Sub(e.ts) < staleEntryTTL {
			out[model] = LoadProgress{Stage: e.stage, Value: e.value, Percent: e.percent}
		}
	}
	return out
}

func (r *Runtime) PrefillProgress(ctx context.Context, model string) (int64, bool) {
	base, key, ok := r.managedRoot()
	if !ok {
		return 0, false
	}
	slots, err := r.managedGetJSON(ctx, base, key, "/slots?"+childQuery(model), prefillTimeout)
	if err != nil {
		return 0, false
	}
	list, _ := slots.([]any)
	var best int64
	for _, slot := range list {
		m, ok := slot.(map[string]any)
		if !ok || !jsonTruthy(m["is_processing"]) {
			continue
		}
		switch n := m["n_prompt_tokens_processed"].(type) {
		case float64:
			best = max(best, int64(n))
		case string:
			if v, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
				best = max(best, v)
			}
		case bool:
			if n {
				best = max(best, 1)
			}
		}
	}
	return best, best > 0
}
