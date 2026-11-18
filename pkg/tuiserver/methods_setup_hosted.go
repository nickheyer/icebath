package tuiserver

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/setupflow"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var hostedFlowRegistry = &handlerRegistry{}

func init() {
	hostedFlowRegistry.scopedMethod("setup.hosted.start", catch(5046, hostedFlowStart))
	hostedFlowRegistry.scopedMethod("setup.hosted.poll", catch(5047, hostedFlowPoll))
	hostedFlowRegistry.scopedMethod("setup.hosted.answer", catch(5048, hostedFlowAnswer))
	hostedFlowRegistry.scopedMethod("setup.hosted.cancel", catch(5049, hostedFlowCancel))
	hostedFlowRegistry.install()
	addLongHandlers("setup.hosted.poll")
}

// Question a hosted flow waits on, with the channel its answer goes back through
type flowQuestion struct {
	id     int
	prompt provider.HostedPrompt
	reply  chan provider.HostedAnswer
}

// One hosted setup flow, its printed lines, open question, ending model and starting transport
type flowJob struct {
	id       string
	lines    *setupJob
	cancel   context.CancelFunc
	owner    tuigateway.Transport
	mu       sync.Mutex
	seq      int
	question *flowQuestion
	result   provider.HostedModelResult
	progress string
}

// Output of a hosted section, its lines kept for polling and the line a progress bar rewrites kept apart
type flowOutput struct{ job *flowJob }

func (o flowOutput) Write(p []byte) (int, error) { return o.job.lines.Write(p) }

// Keeps the line a progress bar is rewriting for the next poll
func (o flowOutput) Progress(text string) {
	o.job.mu.Lock()
	o.job.progress = text
	o.job.mu.Unlock()
}

var (
	flowJobsMu sync.Mutex
	flowJobs   = map[string]*flowJob{}
	flowActive *flowJob
)

// Puts a question to the TUI and waits for its answer, a cancelled job answering every question as backed out
func (j *flowJob) ask(ctx context.Context) provider.SetupAsker {
	return func(p provider.HostedPrompt) provider.HostedAnswer {
		j.mu.Lock()
		if ctx.Err() != nil {
			j.mu.Unlock()
			return provider.HostedAnswer{Cancelled: true}
		}
		j.seq++
		q := &flowQuestion{id: j.seq, prompt: p, reply: make(chan provider.HostedAnswer, 1)}
		j.question = q
		j.mu.Unlock()
		defer func() {
			j.mu.Lock()
			if j.question == q {
				j.question = nil
			}
			j.mu.Unlock()
		}()
		select {
		case answer := <-q.reply:
			return answer
		case <-ctx.Done():
			return provider.HostedAnswer{Cancelled: true}
		}
	}
}

// Wire form of the question the job waits on, nil when it waits on none
func (j *flowJob) pending() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	q := j.question
	if q == nil {
		return nil
	}
	headings := []any{}
	for at, text := range q.prompt.Headings {
		headings = append(headings, map[string]any{"at": at, "text": text})
	}
	return map[string]any{"id": q.id, "kind": q.prompt.Kind, "title": q.prompt.Title, "description": q.prompt.Description,
		"rows": anyStrings(q.prompt.Rows), "headings": headings, "default": q.prompt.Default, "picked": anyInts(q.prompt.Picked),
		"argv": anyStrings(q.prompt.Argv)}
}

func anyStrings(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

func anyInts(list []int) []any {
	out := make([]any, len(list))
	for i, n := range list {
		out[i] = n
	}
	return out
}

// Setup sections and first run modes a hosted job runs besides the model section, assigned in cmd/icebath/wiring.go
var HostedSetupFlows map[string]func(ctx context.Context, ask setupflow.Asker) error

// Runs the model section or a named setup flow on the job, one hosted job or setup action at once
func (j *flowJob) run(ctx context.Context, flow, entry string) {
	setupActionMu.Lock()
	defer setupActionMu.Unlock()
	outcome := false
	defer func() {
		if failure := recover(); failure != nil {
			fmt.Fprintf(j.lines, "%v\n", failure)
		}
		j.lines.finish(outcome)
	}()
	var result provider.HostedModelResult
	var err error
	stopped := false
	if flow == "model" {
		result, err = provider.RunHostedModelSection(ctx, j.ask(ctx), flowOutput{job: j}, entry)
	} else {
		result, stopped, err = provider.RunHostedSetupFlow(ctx, j.ask(ctx), flowOutput{job: j}, HostedSetupFlows[flow])
	}
	if err != nil {
		fmt.Fprintln(j.lines, err.Error())
		return
	}
	j.mu.Lock()
	j.result = result
	j.mu.Unlock()
	outcome = !stopped && ctx.Err() == nil
}

// Starts a hosted setup flow in the background, cancelling a flow still running, and returns its job
func hostedFlowStart(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	h := home.Dir(ctx)
	if config.IsManaged(ctx, h) {
		return rpcErr(rid, 4006, "managed install — the configuration is read-only", nil), nil
	}
	flow, entry := strOr(params["flow"]), strOr(params["entry"])
	switch {
	case flow == "model":
		if !provider.HostedModelEntry(entry) {
			return rpcErr(rid, 4001, "unknown model section entry "+entry, nil), nil
		}
	case HostedSetupFlows[flow] == nil:
		return rpcErr(rid, 4001, "unknown setup flow "+flow, nil), nil
	case entry != "":
		return rpcErr(rid, 4001, "setup flow "+flow+" takes no entry", nil), nil
	}
	jobCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	job := &flowJob{lines: &setupJob{}, cancel: cancel, owner: tuigateway.CurrentTransport(ctx)}
	flowJobsMu.Lock()
	if flowActive != nil {
		flowActive.cancel()
		delete(flowJobs, flowActive.id)
	}
	flowActive = job
	setupJobsMu.Lock()
	setupJobCount++
	id := "flow-" + strconv.Itoa(setupJobCount)
	setupJobsMu.Unlock()
	job.id = id
	flowJobs[id] = job
	flowJobsMu.Unlock()
	go job.run(jobCtx, flow, entry)
	return ok(rid, map[string]any{"job": id}), nil
}

// Job a request names, nil when unknown
func flowJobOf(params map[string]any) (*flowJob, string) {
	id := strOr(params["job"])
	flowJobsMu.Lock()
	defer flowJobsMu.Unlock()
	return flowJobs[id], id
}

// Waits briefly for output, a question or the end of a flow, forgetting the job once it ended
func hostedFlowPoll(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	job, id := flowJobOf(params)
	after, fits := sectionWriteValue("whole", params["after"])
	if job == nil || !fits {
		return rpcErr(rid, 4001, "unknown setup flow job "+id, nil), nil
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		lines, done, outcome := job.lines.since(after.(int))
		var question any
		if pending := job.pending(); pending != nil {
			question = pending
		}
		if len(lines) > 0 || done || question != nil || time.Now().After(deadline) || ctx.Err() != nil {
			job.mu.Lock()
			progress := job.progress
			job.mu.Unlock()
			reply := map[string]any{"lines": lines, "question": question, "done": done, "ok": outcome, "model": "", "provider": "", "changed": false,
				"progress": progress}
			if done {
				job.mu.Lock()
				reply["model"], reply["provider"], reply["changed"] = job.result.Model, job.result.Provider, job.result.Changed
				job.mu.Unlock()
				flowJobsMu.Lock()
				delete(flowJobs, id)
				if flowActive == job {
					flowActive = nil
				}
				flowJobsMu.Unlock()
			}
			return ok(rid, reply), nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Hands the TUI's answer to the question the job waits on
func hostedFlowAnswer(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	job, id := flowJobOf(params)
	qid, fits := sectionWriteValue("whole", params["question"])
	if job == nil || !fits {
		return rpcErr(rid, 4001, "unknown setup flow job "+id, nil), nil
	}
	answer := provider.HostedAnswer{Cancelled: params["cancelled"] == true, Text: strOr(params["text"]), Ran: params["ran"] == true,
		Err: strOr(params["error"])}
	if index, whole := sectionWriteValue("whole", params["index"]); whole {
		answer.Index = index.(int)
	}
	for _, raw := range wireList(params["picked"]) {
		if index, whole := sectionWriteValue("whole", raw); whole {
			answer.Picked = append(answer.Picked, index.(int))
		}
	}
	job.mu.Lock()
	q := job.question
	if q != nil && q.id == qid.(int) {
		job.question = nil
	}
	job.mu.Unlock()
	if q == nil || q.id != qid.(int) {
		return rpcErr(rid, 4001, "setup flow job "+id+" is not waiting on question "+strconv.Itoa(qid.(int)), nil), nil
	}
	q.reply <- answer
	return ok(rid, map[string]any{}), nil
}

// Items of a wire list in either slice type a transport delivers
func wireList(v any) []any {
	switch list := v.(type) {
	case []any:
		return list
	case []int:
		return anyInts(list)
	}
	return nil
}

// Stops a hosted flow, its open question answered as backed out and its waits ended
func hostedFlowCancel(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	job, id := flowJobOf(params)
	if job == nil {
		return rpcErr(rid, 4001, "unknown setup flow job "+id, nil), nil
	}
	job.cancel()
	job.mu.Lock()
	job.question = nil
	job.mu.Unlock()
	return ok(rid, map[string]any{}), nil
}
