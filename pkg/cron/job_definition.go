package cron

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// JobDefinitionFields are the authored keys a foreign store refreshes, every other key is scheduler owned
var JobDefinitionFields = []string{
	"name", "prompt", "skills", "skill", "model", "provider", "base_url",
	"script", "no_agent", "monitor_script", "monitor_url", "context_from",
	"schedule", "schedule_display", "deliver", "origin", "enabled_toolsets",
	"workdir", "attach_to_session", "reasoning_effort", "failure_deliver",
	"timezone", "grace_minutes", "catch_up",
}

// Installs the cron store access the profile distribution installer reaches through hooks
func init() {
	profiles.CronDistributionHooks.LoadJobs = distributionLoadJobs
	profiles.CronDistributionHooks.ImportJobDefinitions = func(home string, shipped *pyjson.Object, pausedReason string) error {
		return openCronStore(home, nil).importJobDefinitions(shipped, pausedReason)
	}
}

// Jobs of the store under home as plain decoded records
func distributionLoadJobs(home string) ([]any, error) {
	jobs, err := openCronStore(home, nil).LoadJobs()
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(jobs))
	for _, job := range jobs {
		raw, err := marshalNoEscape(job)
		if err != nil {
			return nil, err
		}
		record, err := pyjson.DecodeWTF8(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, nil
}

// Ordered object of a JSON shaped record, the Python TypeError or AttributeError a merge raises for any other shape
func recordObject(v any) (*pyjson.Object, error) {
	raw, err := marshalNoEscape(v)
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.Loads(string(raw))
	if err != nil {
		return nil, err
	}
	if obj, ok := decoded.(*pyjson.Object); ok {
		return obj, nil
	}
	return nil, recordShapeFailure(decoded)
}

// Python error of a non dict record reaching the membership test, the field lookup or the get call
func recordShapeFailure(v any) error {
	switch x := v.(type) {
	case string:
		for _, key := range JobDefinitionFields {
			if strings.Contains(x, key) {
				return &recordShapeError{"string indices must be integers, not 'str'"}
			}
		}
	case []any:
		for _, item := range x {
			for _, key := range JobDefinitionFields {
				if pyrepr.Equal(item, key) {
					return &recordShapeError{"list indices must be integers or slices, not str"}
				}
			}
		}
	default:
		return &recordShapeError{fmt.Sprintf("argument of type '%s' is not iterable", jsonTypeName(v))}
	}
	return &recordShapeError{fmt.Sprintf("'%s' object has no attribute 'get'", jsonTypeName(v))}
}

// TypeError or AttributeError of a malformed record, never labelled with the job name
type recordShapeError struct{ msg string }

func (e *recordShapeError) Error() string { return e.msg }

// Python type name of a decoded JSON value
func jsonTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case float64:
		return "float"
	case []any:
		return "list"
	case *pyjson.Object:
		return "dict"
	}
	return "int"
}

// Key order independent text of one record value, absent and null alike, for dict equality
func canonicalValue(v any, present bool) (string, error) {
	if !present {
		return "null", nil
	}
	raw, err := marshalNoEscape(v)
	if err != nil {
		return "", err
	}
	plain, err := pyjson.DecodeWTF8(raw)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(plain)
	return string(out), err
}

// Dict get of a falsy-defaulted record value, the Python AttributeError for a truthy non dict
func objectGet(v any, key string) (any, error) {
	if !pyrepr.Truthy(v) {
		return nil, nil
	}
	obj, ok := v.(*pyjson.Object)
	if !ok {
		return nil, &recordShapeError{fmt.Sprintf("'%s' object has no attribute 'get'", jsonTypeName(v))}
	}
	got, _ := obj.Get(key)
	return got, nil
}

// Refreshes authored fields from authored while keeping the scheduler owned state of local
func (s *Store) mergeJobDefinition(local *Job, authored any) (*Job, error) {
	localObj, err := recordObject(local)
	if err != nil {
		return nil, err
	}
	authoredObj, err := recordObject(authored)
	if err != nil {
		return nil, err
	}
	definition := map[string]bool{}
	for _, key := range JobDefinitionFields {
		definition[key] = true
	}
	merged := pyjson.NewObject()
	for _, key := range localObj.Keys {
		if !definition[key] {
			v, _ := localObj.Get(key)
			merged.Set(key, v)
		}
	}
	for _, key := range JobDefinitionFields {
		if v, ok := authoredObj.Get(key); ok {
			merged.Set(key, v)
		}
	}
	localRepeat, _ := localObj.Get("repeat")
	var completed any = int64(0)
	if obj, ok := localRepeat.(*pyjson.Object); ok {
		if v, has := obj.Get("completed"); has {
			completed = v
		}
	} else if _, err := objectGet(localRepeat, "completed"); err != nil {
		return nil, err
	}
	authoredRepeat, _ := authoredObj.Get("repeat")
	times, err := objectGet(authoredRepeat, "times")
	if err != nil {
		return nil, err
	}
	repeat := pyjson.NewObject()
	repeat.Set("completed", completed)
	repeat.Set("times", times)
	merged.Set("repeat", repeat)
	if text, ok := merged.Vals["schedule"].(string); ok {
		sched, err := s.parseSchedule(text)
		if err != nil {
			return nil, err
		}
		merged.Set("schedule", sched)
	}
	changed := false
	for _, key := range []string{"schedule", "timezone"} {
		before, hadBefore := localObj.Get(key)
		after, hasAfter := merged.Get(key)
		a, err := canonicalValue(before, hadBefore)
		if err != nil {
			return nil, err
		}
		b, err := canonicalValue(after, hasAfter)
		if err != nil {
			return nil, err
		}
		changed = changed || a != b
	}
	raw, err := marshalNoEscape(merged)
	if err != nil {
		return nil, err
	}
	job := &Job{}
	if err := json.Unmarshal(raw, job); err != nil {
		return nil, err
	}
	if !changed {
		return job, nil
	}
	job.PendingSlot = nil
	quotaHoldClearState(job)
	if !isJobRunnable(job) {
		job.NextRunAt = nil
		return job, nil
	}
	updates := map[string]any{"schedule": job.Schedule}
	if display, ok := authoredObj.Get("schedule_display"); ok {
		updates["schedule_display"] = display
	}
	label := job.ID
	if label == "" {
		label = "imported job"
	}
	if err := s.applyScheduleUpdate(job, updates, label); err != nil {
		return nil, err
	}
	return job, nil
}

// Merges one authored record, naming the job in any refusal
func (s *Store) mergeOrName(local *Job, authored any) (*Job, error) {
	merged, err := s.mergeJobDefinition(local, authored)
	var shape *recordShapeError
	if err == nil || errors.As(err, &shape) {
		return merged, err
	}
	obj, oerr := recordObject(authored)
	if oerr != nil {
		return nil, oerr
	}
	name, _ := obj.Get("name")
	if !pyrepr.Truthy(name) {
		name = local.ID
	}
	return nil, fmt.Errorf("cron job %s: %w", pyrepr.Repr(name), err)
}

// Merges shipped job id to authored record into this store under its lock, new ids arriving paused
func (s *Store) importJobDefinitions(shipped *pyjson.Object, pausedReason string) error {
	now := FormatISO(s.clk.now())
	return s.withJobsLock(func(tx *jobsTx) error {
		locals, err := s.loadJobs(tx)
		if err != nil {
			return err
		}
		taken := map[string]bool{}
		merged := make([]*Job, 0, len(locals)+shipped.Len())
		for _, local := range locals {
			incoming, ok := shipped.Get(local.ID)
			if !ok || taken[local.ID] || !local.hasKey("id") {
				merged = append(merged, local)
				continue
			}
			taken[local.ID] = true
			if incoming == nil {
				merged = append(merged, local)
				continue
			}
			job, err := s.mergeOrName(local, incoming)
			if err != nil {
				return err
			}
			merged = append(merged, job)
		}
		for _, id := range shipped.Keys {
			if taken[id] {
				continue
			}
			seed := pyjson.NewObject()
			seed.Set("id", id)
			seed.Set("enabled", false)
			seed.Set("state", "paused")
			seed.Set("paused_at", now)
			seed.Set("paused_reason", pausedReason)
			seed.Set("created_at", now)
			seed.Set("next_run_at", nil)
			raw, err := marshalNoEscape(seed)
			if err != nil {
				return err
			}
			local := &Job{}
			if err := json.Unmarshal(raw, local); err != nil {
				return err
			}
			job, err := s.mergeOrName(local, shipped.Vals[id])
			if err != nil {
				return err
			}
			merged = append(merged, job)
		}
		return s.saveJobsLocked(tx, merged, nil, false)
	})
}
