package packages

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Receipts kept of past operations
const receiptsKept = 20

// Record of one package manager operation: its steps, warnings, any refusal, and how it ended. pm status prints the newest
type Receipt struct {
	Schema      int        `json:"schema"`
	Kind        string     `json:"kind"`
	StartedAt   string     `json:"started_at"`
	Steps       []Step     `json:"steps"`
	VenvRebuild *Verdict   `json:"venv_rebuild"`
	FeatureList []string   `json:"feature_list"`
	Outcome     *string    `json:"outcome"`
	Warnings    []Note     `json:"warnings"`
	Refusal     *Refusal   `json:"refusal"`
	ExitCode    int        `json:"exit_code"`
	FinishedAt  string     `json:"finished_at,omitempty"`
	mu          sync.Mutex `json:"-"`
}

type Step struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	At     string `json:"at"`
}

type Verdict struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
}

type Note struct {
	Message string `json:"message"`
	At      string `json:"at"`
}

type Refusal struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
	At     string `json:"at"`
}

type receiptKey struct{}

// Context recording into a new receipt of the kind
func beginReceipt(ctx context.Context, kind string) (context.Context, *Receipt) {
	r := &Receipt{Schema: 1, Kind: kind, StartedAt: now(), Steps: []Step{}, Warnings: []Note{}}
	return context.WithValue(ctx, receiptKey{}, r), r
}

// Receipt the context records into, nil outside an operation
func receiptOf(ctx context.Context) *Receipt {
	r, _ := ctx.Value(receiptKey{}).(*Receipt)
	return r
}

func now() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000-07:00")
}

func (r *Receipt) step(name string, ok bool, detail string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Steps = append(r.Steps, Step{Name: name, OK: ok, Detail: detail, At: now()})
}

func (r *Receipt) warn(message string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Warnings = append(r.Warnings, Note{Message: message, At: now()})
}

func (r *Receipt) refuse(code, detail string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Refusal = &Refusal{Code: code, Detail: detail, At: now()}
}

func (r *Receipt) rebuilt(ok bool, reason string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.VenvRebuild = &Verdict{OK: ok, Reason: reason}
}

func (r *Receipt) features(extras []string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.FeatureList = slices.Clone(extras)
}

// Folder of the profile's receipts
func receiptsDir(cfg *config.Config) string {
	return filepath.Join(cfg.ProfileDir(cfg.Profile()), "logs", "update_receipts")
}

// Ends the receipt and writes it as the profile's latest, the oldest past the limit removed
func (r *Receipt) finish(cfg *config.Config, code int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	outcome := "ok"
	if code != 0 {
		outcome = "failed"
	}
	r.Outcome, r.ExitCode, r.FinishedAt = &outcome, code, now()
	r.mu.Unlock()
	dir := receiptsDir(cfg)
	release, err := fsx.Lock(context.Background(), filepath.Join(dir, ".pm-write.lock"), -1)
	if err != nil {
		return
	}
	defer release()
	name := fmt.Sprintf("pm_%s-%s-%d-%s.json", time.Now().UTC().Format("20060102T150405Z"), r.Kind, os.Getpid(), strs.Random(16))
	if writeJSON(filepath.Join(dir, name), r) != nil || writeJSON(filepath.Join(dir, "latest.json"), r) != nil {
		return
	}
	entries, _ := os.ReadDir(dir)
	var receipts []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pm_") && strings.HasSuffix(e.Name(), ".json") {
			receipts = append(receipts, e.Name())
		}
	}
	slices.Sort(receipts)
	for _, stale := range receipts[:max(0, len(receipts)-receiptsKept)] {
		os.Remove(filepath.Join(dir, stale))
	}
}

// Newest receipt of the profile as written, false when there is none
func (h *Packages) latestReceipt(cfg *config.Config) ([]byte, bool) {
	data, err := os.ReadFile(filepath.Join(receiptsDir(cfg), "latest.json"))
	return data, err == nil && len(strings.TrimSpace(string(data))) > 0
}
