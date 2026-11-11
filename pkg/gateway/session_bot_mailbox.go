package gateway

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Mailbox home and owning session of one unsettled bot receipt
type botReceiptOwner struct {
	home      string
	sessionID string
}

// Mailbox record read by a bulk scan with its ticket path
type botMailboxRecord struct {
	Path   string
	Record map[string]any
}

// Pre admission pin a bulk owner scan collected with its modification time
type botMailboxPin struct {
	Path   string
	Record map[string]any
	MTime  float64
}

// Expected status of the stored receipt a guarded write requires, nil for an absent one
type receiptExpect struct {
	status any
}

var botPinFields = map[string]bool{"profile_home": true, "session_id": true, "principal_id": true, "operator": true}

// Runs body as the authority's one mailbox operation at a time, counted as owner work until it ends
func mailboxLock(ctx context.Context, a *SessionAuthority, body func(ctx context.Context) error) error {
	if err := a.botMailboxLock.Lock(ctx); err != nil {
		return err
	}
	defer a.botMailboxLock.Unlock()
	task := &trackedTask{done: make(chan struct{})}
	a.mu.Lock()
	a.mutationTasks[task] = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.mutationTasks, task)
		a.mu.Unlock()
		close(task.done)
		persistWorkCount(a)
	}()
	return body(ctx)
}

// Runs physical mailbox work to its end before a cancelled caller sees its cancellation
func mailboxIO[T any](ctx context.Context, operation func(ctx context.Context) (T, error)) (T, error) {
	result, err := operation(context.WithoutCancel(ctx))
	if cancelled := ctx.Err(); cancelled != nil {
		var zero T
		return zero, cancelled
	}
	return result, err
}

// Mailbox root and stored receipt of a delivery id, nil when absent
func readReceipt(ctx context.Context, home, key string) (string, map[string]any, error) {
	type found struct {
		root   string
		record map[string]any
	}
	out, err := mailboxIO(ctx, func(ioCtx context.Context) (found, error) {
		var result found
		err := lockedMailbox(ioCtx, home, func(root string) error {
			record, err := readTicket(filepath.Join(root, key+".json"))
			result = found{root, record}
			return err
		})
		return result, err
	})
	return out.root, out.record, err
}

// Merges a record into its stored receipt, false when the stored status is not the expected one
func writeReceiptLocked(ctx context.Context, home string, record map[string]any, expect *receiptExpect) (bool, error) {
	written := false
	err := lockedMailbox(ctx, home, func(root string) error {
		deliveryID, ok := record["delivery_id"]
		if !ok {
			return &pyKeyError{"delivery_id"}
		}
		path := filepath.Join(root, pyrepr.Str(deliveryID)+".json")
		current, err := readTicket(path)
		if err != nil {
			return err
		}
		if expect != nil && !pyjson.Equal(current["status"], expect.status) {
			return nil
		}
		if current != nil && pyTruthy(current["principal_id"]) {
			conflict := false
			for _, field := range []string{"profile_home", "session_id", "principal_id", "message", "author"} {
				if !pyjson.Equal(current[field], record[field]) {
					conflict = true
				}
			}
			if !pyjson.Equal(categoryOf(current), categoryOf(record)) ||
				(pyTruthy(current["admission_id"]) && !pyjson.Equal(current["admission_id"], record["admission_id"])) {
				conflict = true
			}
			if conflict {
				return runtimeRefusal("admission_conflict")
			}
		}
		merged := map[string]any{}
		maps.Copy(merged, current)
		maps.Copy(merged, record)
		if current == nil || !pyjson.Equal(merged, current) {
			if err := writeTicket(ctx, path, merged); err != nil {
				return err
			}
		}
		written = true
		return nil
	})
	return written, err
}

// Notification category of a receipt, result when it names none
func categoryOf(record map[string]any) any {
	if category, ok := record["notification_category"]; ok {
		return category
	}
	return "result"
}

// Merges a record into its stored receipt under the mailbox lock, guarded by the expected status when given
func writeReceipt(ctx context.Context, home string, record map[string]any, expect *receiptExpect) (bool, error) {
	return mailboxIO(ctx, func(ioCtx context.Context) (bool, error) {
		return writeReceiptLocked(ioCtx, home, record, expect)
	})
}

// Drops a pre admission pin the FIFO refused while it is still that pin, so the same id retry admits
func withdrawPin(ctx context.Context, home, key string) (bool, error) {
	return mailboxIO(ctx, func(ioCtx context.Context) (bool, error) {
		withdrawn := false
		err := lockedMailbox(ioCtx, home, func(root string) error {
			path := filepath.Join(root, key+".json")
			current, err := readTicket(path)
			if err != nil {
				return err
			}
			if current != nil && current["status"] == "ambiguous" && !pyTruthy(current["admission_id"]) {
				if err := os.Remove(path); err != nil {
					return err
				}
				withdrawn = true
			}
			return nil
		})
		return withdrawn, err
	})
}

// Restores the queued legacy ticket a pre admission pin was laid over, nil when the pin is gone or changed
func requeuePin(ctx context.Context, home, key string) (map[string]any, error) {
	return mailboxIO(ctx, func(ioCtx context.Context) (map[string]any, error) {
		var ticket map[string]any
		err := lockedMailbox(ioCtx, home, func(root string) error {
			path := filepath.Join(root, key+".json")
			current, err := readTicket(path)
			if err != nil {
				return err
			}
			if _, legacy := current["owner"]; current == nil || current["status"] != "ambiguous" || pyTruthy(current["admission_id"]) || !legacy {
				return nil
			}
			restored := map[string]any{}
			for k, v := range current {
				if !botPinFields[k] {
					restored[k] = v
				}
			}
			restored["status"] = "queued"
			if err := writeTicket(ioCtx, path, restored); err != nil {
				return err
			}
			ticket = restored
			return nil
		})
		return ticket, err
	})
}

// Settles a pre admission pin the FIFO refused for good as a terminal failed receipt
func refusePin(ctx context.Context, home, key string) error {
	_, err := mailboxIO(ctx, func(ioCtx context.Context) (struct{}, error) {
		return struct{}{}, lockedMailbox(ioCtx, home, func(root string) error {
			path := filepath.Join(root, key+".json")
			current, err := readTicket(path)
			if err != nil {
				return err
			}
			if current != nil && current["status"] == "ambiguous" && !pyTruthy(current["admission_id"]) {
				current["status"] = "failed"
				current["refused"] = true
				current["reply"] = ""
				current["error"] = "Bot Chat delivery was refused"
				current["reason"] = botReasonAgentBlocked
				return writeTicket(ioCtx, path, current)
			}
			return nil
		})
	})
	return err
}

// Failure reason of a delivery the agent's own policy refused
const botReasonAgentBlocked = "agent_blocked"

// Bulk mailbox scan under its lock, an owner scan collecting pre admission pins in pins
func scanReceipts(ctx context.Context, home string, pins *[]botMailboxPin) (string, []botMailboxRecord, error) {
	type scanned struct {
		root    string
		records []botMailboxRecord
	}
	out, err := mailboxIO(ctx, func(ioCtx context.Context) (scanned, error) {
		var result scanned
		err := lockedMailbox(ioCtx, home, func(root string) error {
			result = scanned{root, scanBotRecords(root, pins)}
			return nil
		})
		return result, err
	})
	return out.root, out.records, err
}

// Indexes an unsettled receipt by delivery id for owner publication to re-arm, a terminal one leaving the index
func rememberReceipt(a *SessionAuthority, home string, record map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.botReceiptIndex == nil {
		a.botReceiptIndex = map[string]botReceiptOwner{}
	}
	key := pyrepr.Str(record["delivery_id"])
	switch record["status"] {
	case "settled", "failed", "cancelled":
		delete(a.botReceiptIndex, key)
	default:
		a.botReceiptIndex[key] = botReceiptOwner{home, pyrepr.Str(record["session_id"])}
	}
}

// Keeps a receipt projection task reachable until it ends, logging the failure of one that was not cancelled
func trackReceiptTask(a *SessionAuthority, task *adapterTask) {
	a.mu.Lock()
	if a.botReceiptTasks == nil {
		a.botReceiptTasks = map[*adapterTask]bool{}
	}
	a.botReceiptTasks[task] = true
	a.mu.Unlock()
	go func() {
		<-task.done
		a.mu.Lock()
		delete(a.botReceiptTasks, task)
		a.mu.Unlock()
		if task.err != nil && !errors.Is(task.err, context.Canceled) {
			slog.Warn("Bot receipt projection failed: " + pyrepr.ExceptionName(task.err))
		}
	}()
}

// Re-arms the paused receipts of a session from owner publication, marking a running refresh dirty
func wakeBotReceipts(ctx context.Context, a *SessionAuthority, sid string) {
	a.mu.Lock()
	if a.botReceiptRefreshes == nil {
		a.botReceiptRefreshes = map[string]*adapterTask{}
		a.botReceiptDirty = map[string]bool{}
	}
	type start struct{ home, key string }
	var starts []start
	for key, owner := range a.botReceiptIndex {
		if owner.sessionID != sid {
			continue
		}
		if task, pending := a.botReceiptRefreshes[key]; pending && !task.finished() {
			a.botReceiptDirty[key] = true
		} else {
			starts = append(starts, start{owner.home, key})
		}
	}
	a.mu.Unlock()
	for _, s := range starts {
		startReceiptRefresh(ctx, a, s.home, s.key)
	}
}

// Refreshes one receipt until no owner publication arrived during its last pass
func startReceiptRefresh(ctx context.Context, a *SessionAuthority, home, key string) {
	registered := make(chan struct{})
	var task *adapterTask
	task = spawnAdapterTask(ctx, func(taskCtx context.Context) error {
		<-registered
		defer func() {
			a.mu.Lock()
			if a.botReceiptRefreshes[key] == task {
				delete(a.botReceiptRefreshes, key)
				delete(a.botReceiptDirty, key)
			}
			a.mu.Unlock()
		}()
		for {
			a.mu.Lock()
			delete(a.botReceiptDirty, key)
			a.mu.Unlock()
			if err := refreshBotReceipt(taskCtx, a, home, key); err != nil {
				return err
			}
			a.mu.Lock()
			_, indexed := a.botReceiptIndex[key]
			dirty := a.botReceiptDirty[key]
			a.mu.Unlock()
			if !dirty || !indexed {
				return nil
			}
		}
	})
	a.mu.Lock()
	a.botReceiptRefreshes[key] = task
	a.mu.Unlock()
	close(registered)
	trackReceiptTask(a, task)
}
