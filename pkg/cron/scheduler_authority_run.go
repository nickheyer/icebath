package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Canonical owner doors, the seams tests replace the way pytest patches gateway.session_cron and the gateway client
var (
	cronOwnerForHome   = gateway.CronOwnerForHome
	cronOwnerOperation = gateway.CronOwnerOperation
	connectGateway     = func(ctx context.Context) (cronRPCClient, error) { return gateway.ConnectGateway(ctx) }
)

// Gateway RPC connection a remote canonical run observes through
type cronRPCClient interface {
	RPC(ctx context.Context, method string, params map[string]any) (map[string]any, error)
	Close() error
}

// Failure outcome of a canonical run with the exception class and message
func canonicalFailure(jobID string, err error) jobRunOutcome {
	text := fmt.Sprintf("%s: %v", pyExceptionName(err), err)
	return jobRunOutcome{Success: false, Output: fmt.Sprintf("# Cron Job: %s (FAILED)\n\n%s\n", jobID, text), Error: &text}
}

// Outcome of a terminal result list of success, output, answer and error
func outcomeFromResult(v any) (jobRunOutcome, error) {
	list, ok := v.([]any)
	if !ok || len(list) != 4 {
		return jobRunOutcome{}, fmt.Errorf("canonical cron result is not a four item list: %s", pyrepr.Repr(v))
	}
	out := jobRunOutcome{Success: pyTruthy(list[0]), Output: pyStrOrEmpty(list[1]), FinalResponse: pyStrOrEmpty(list[2])}
	if list[3] != nil {
		e := pyrepr.Str(list[3])
		out.Error = &e
	}
	return out, nil
}

// Applies a mapping of job fields onto the live job record
func applyJobFlags(job *Job, flags any) error {
	m, _ := flags.(map[string]any)
	if obj, ok := flags.(*pyjson.Object); ok {
		m = map[string]any{}
		for _, k := range obj.Keys {
			m[k], _ = obj.Get(k)
		}
	}
	for k, v := range m {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err := job.setField(k, raw); err != nil {
			return err
		}
	}
	return nil
}

// Admits the job through the canonical owner and observes it to its terminal result, journaled for recovery
func (s *Store) runCanonicalJob(ctx context.Context, job *Job, extra *string, cancel CancelSource, executionID *string) (jobRunOutcome, error) {
	requestID := ""
	if executionID != nil && *executionID != "" {
		requestID = *executionID
	} else if job.ExecutionID != nil && *job.ExecutionID != "" {
		requestID = *job.ExecutionID
	} else {
		requestID = uuid4Hex()
		job.ExecutionID = strPtr(requestID)
	}
	params := pyjson.NewObject()
	params.Set("job_id", job.ID)
	params.Set("request_id", requestID)
	var extraVal any
	if extra != nil {
		extraVal = *extra
	}
	params.Set("extra_prompt", extraVal)
	root := filepath.Join(s.home, "cron", "admissions")
	journal := s.journalPath(job.ID, requestID)
	record := pyjson.NewObject()
	record.Set("params", params)
	record.Set("receipt", nil)
	_, statErr := os.Stat(journal)
	attempted := statErr == nil
	if attempted {
		loaded, err := readJournalRecord(journal)
		if err != nil {
			return jobRunOutcome{}, err
		}
		stored, _ := loaded.Get("params")
		if !jsonEqual(stored, params) {
			return jobRunOutcome{}, &CronExecutionUnknown{Msg: "cron admission identity conflict"}
		}
		record = loaded
	}
	authority, owned := cronOwnerForHome(s.home)
	save := func() error {
		if err := os.MkdirAll(root, 0o777); err != nil {
			return err
		}
		return s.writeAdmissionJournal(ctx, journal, record)
	}
	paramMap := map[string]any{"job_id": job.ID, "request_id": requestID, "extra_prompt": extraVal}
	observe := func(call func(op string, data map[string]any) (map[string]any, error)) (jobRunOutcome, error) {
		attempted = true
		if err := save(); err != nil {
			return jobRunOutcome{}, err
		}
		receipt, err := call("submit", paramMap)
		if err != nil {
			var rpcErr *gateway.GatewayRPCError
			if !owned && !errors.As(err, &rpcErr) {
				return jobRunOutcome{}, err
			}
			state, rerr := call("recover", paramMap)
			if rerr != nil {
				return jobRunOutcome{}, rerr
			}
			switch state["status"] {
			case "missing":
				failure := canonicalFailure(job.ID, err)
				record.Set("refusal", []any{false, failure.Output, "", *failure.Error})
				snapshot, jerr := jobObject(job)
				if jerr != nil {
					return jobRunOutcome{}, jerr
				}
				record.Set("job", snapshot)
				if serr := save(); serr != nil {
					return jobRunOutcome{}, serr
				}
				return failure, nil
			case "queued", "started", "terminal":
				receipt = map[string]any{"session_id": state["session_id"], "admission_id": state["admission_id"]}
			default:
				return jobRunOutcome{}, err
			}
		}
		record.Set("receipt", receipt)
		if err := save(); err != nil {
			return jobRunOutcome{}, err
		}
		delay := 100 * time.Millisecond
		var progress any
		for {
			if cancel != nil && cancel.IsSet() {
				if _, err := call("cancel", receipt); err != nil {
					return jobRunOutcome{}, err
				}
			}
			state, err := call("status", receipt)
			if err != nil {
				return jobRunOutcome{}, err
			}
			if p := state["progress_at"]; p != nil && !jsonEqual(p, progress) {
				progress = p
				if _, err := openExecutionsLedger(s).TouchProgress(ctx, requestID); err != nil {
					return jobRunOutcome{}, err
				}
			}
			switch state["status"] {
			case "terminal":
				if err := applyJobFlags(job, state["job_flags"]); err != nil {
					return jobRunOutcome{}, err
				}
				return outcomeFromResult(state["result"])
			case "unknown":
				return jobRunOutcome{}, &CronExecutionUnknown{Msg: "unknown_execution: cron admission was not replayed"}
			}
			select {
			case <-ctx.Done():
				return jobRunOutcome{}, ctx.Err()
			case <-time.After(delay):
			}
			if delay *= 2; delay > 2*time.Second {
				delay = 2 * time.Second
			}
		}
	}
	var out jobRunOutcome
	var err error
	if owned {
		out, err = observe(func(op string, data map[string]any) (map[string]any, error) {
			return cronOwnerOperation(ctx, authority, op, data)
		})
	} else {
		var client cronRPCClient
		if client, err = connectGateway(ctx); err == nil {
			out, err = observe(func(op string, data map[string]any) (map[string]any, error) {
				return client.RPC(ctx, "cron."+op, data)
			})
			if cerr := client.Close(); err == nil && cerr != nil {
				err = cerr
			}
		}
	}
	if err == nil {
		return out, nil
	}
	if attempted {
		return jobRunOutcome{}, &CronExecutionUnknown{Msg: fmt.Sprintf("Cron execution unverified; reconcile %s: %v", journal, err)}
	}
	return canonicalFailure(job.ID, err), nil
}

func readJournalRecord(path string) (*pyjson.Object, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	v, err := pyjson.Loads(string(trimBOM(raw)))
	if err != nil {
		return nil, err
	}
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("cron journal %s is not an object", path)
	}
	return obj, nil
}

func trimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xef && b[1] == 0xbb && b[2] == 0xbf {
		return b[3:]
	}
	return b
}

// Mapping form of a decoded JSON object
func objectMap(v any) map[string]any {
	switch o := v.(type) {
	case map[string]any:
		return o
	case *pyjson.Object:
		m := map[string]any{}
		for _, k := range o.Keys {
			m[k], _ = o.Get(k)
		}
		return m
	}
	return nil
}

// Settles every admission journal whose owner is gone, replaying terminal results and pausing unknown ones
func (s *Store) reconcilePendingJournals(ctx context.Context, allowConnect bool) error {
	journals, _ := filepath.Glob(filepath.Join(s.home, "cron", "admissions", "*.json"))
	sort.Strings(journals)
	for _, journal := range journals {
		if err := s.reconcileJournal(ctx, journal, allowConnect); err != nil {
			s.log.Warn(fmt.Sprintf("Cron receipt recovery deferred: %s", journal), "error", err)
		}
	}
	return nil
}

func (s *Store) reconcileJournal(ctx context.Context, journal string, allowConnect bool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	record, err := readJournalRecord(journal)
	if err != nil {
		return err
	}
	paramsVal, ok := record.Get("params")
	if !ok {
		return errors.New("'params'")
	}
	params := objectMap(paramsVal)
	jobID, requestID := pyStrOrEmpty(params["job_id"]), pyStrOrEmpty(params["request_id"])
	if journal != s.journalPath(jobID, requestID) {
		return errors.New("cron journal identity conflict")
	}
	ledger := openExecutionsLedger(s)
	if live, err := ledger.OwnerLive(ctx, requestID, jobID); err != nil || live {
		return err
	}
	row, err := ledger.Get(ctx, requestID)
	if err != nil {
		return err
	}
	if row != nil && row["job_id"] == jobID && FirerFinishedStates[pyStrOrEmpty(row["status"])] {
		_ = os.Remove(journal)
		return nil
	}
	var state map[string]any
	if refusal, ok := record.Get("refusal"); ok {
		jobVal, _ := record.Get("job")
		state = map[string]any{"status": "terminal", "result": refusal, "job": jobVal}
	} else {
		authority, owned := cronOwnerForHome(s.home)
		if !owned && !allowConnect {
			return nil
		}
		if owned {
			rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			state, err = cronOwnerOperation(rctx, authority, "recover", params)
			cancel()
		} else {
			var client cronRPCClient
			if client, err = connectGateway(ctx); err == nil {
				state, err = client.RPC(ctx, "cron.recover", params)
				if cerr := client.Close(); err == nil && cerr != nil {
					err = cerr
				}
			}
		}
		if err != nil {
			return err
		}
	}
	if state["status"] == "missing" {
		current, err := s.getJob(home.WithOverride(ctx, s.home), jobID)
		if err != nil {
			return err
		}
		if current == nil {
			_ = os.Remove(journal)
			return nil
		}
		text := "Cron admission missing: the owner never admitted this fire"
		state = map[string]any{"status": "terminal", "job": current, "result": []any{false, fmt.Sprintf("# Cron Job: %s (FAILED)\n\n%s\n", jobID, text), "", text}}
	}
	if state["status"] != "terminal" {
		if pr, _ := record.Get("pause_recorded"); state["status"] == "unknown" && !pyTruthy(pr) {
			reason := "Canonical cron unknown; no automatic re-execution"
			paused, err := s.pauseJob(home.WithOverride(ctx, s.home), jobID, &reason)
			if err != nil {
				return err
			}
			record.Set("pause_recorded", pauseIdentity(paused))
			return s.writeAdmissionJournal(ctx, journal, record)
		}
		return nil
	}
	outcome, err := outcomeFromResult(state["result"])
	if err != nil {
		return err
	}
	job, err := jobFromState(state["job"])
	if err != nil {
		return err
	}
	if err := applyJobFlags(job, state["job_flags"]); err != nil {
		return err
	}
	job.ExecutionID = strPtr(requestID)
	success, errText, declared := applyAgentFailureMarker(job, outcome.Success, outcome.Error, outcome.FinalResponse)
	d := &runDelivery{job: job, success: success, err: errText, agentDeclared: declared}
	fenced := job.Clone()
	fenced.FireClaim = nil
	fence := s.newFireOwnership(fenced, nil, nil, nil)
	if err := s.saveComposeDeliver(ctx, d, fence, outcome.FinalResponse, outcome.Output, runOneJobOptions{}, nil); err != nil {
		return err
	}
	failEmptyResponse(d, outcome.FinalResponse)
	finished, err := s.finishCompletedRun(ctx, d, nil, requestID, true)
	if err != nil {
		return err
	}
	if finished {
		pr, _ := record.Get("pause_recorded")
		return s.liftPause(home.WithOverride(ctx, s.home), jobID, pr)
	}
	return nil
}

// Job record of a recovery state, decoded from its JSON form
func jobFromState(v any) (*Job, error) {
	if j, ok := v.(*Job); ok {
		return j.Clone(), nil
	}
	raw, err := pyjson.Dumps(v)
	if err != nil {
		return nil, err
	}
	job := &Job{}
	if err := json.Unmarshal([]byte(raw), job); err != nil {
		return nil, err
	}
	return job, nil
}

// Admission journal recovery the tick runs, the seam tests replace the way pytest patches the module attribute
var reconcilePending = func(ctx context.Context, s *Store, allowConnect bool) error {
	return s.reconcilePendingJournals(ctx, allowConnect)
}
