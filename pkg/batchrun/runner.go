package batchrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	termutil "github.com/nickheyer/icebath/pkg/utils/term"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// Options are the settings of one batch run
type Options struct {
	DatasetFile  string
	BatchSize    int
	RunName      string
	Distribution string
	// Tool calling iterations one prompt may take
	MaxIterations int
	BaseURL       string
	APIKey        string
	Model         string
	NumWorkers    int
	Verbose       bool
	// System prompt the agents run under that is never written to a trajectory
	EphemeralSystemPrompt string
	LogPrefixChars        int
	ProvidersAllowed      []string
	ProvidersIgnored      []string
	ProvidersOrder        []string
	ProviderSort          string
	ReasoningConfig       map[string]any
	PrefillMessages       []any
	// First entries of the dataset to keep, zero keeping all of them
	MaxSamples int
}

// Runner holds the dataset, batches and output paths of one batch run
type Runner struct {
	o           Options
	out         io.Writer
	home        string
	cwd         string
	datasetFile string
	outputDir   string
	checkpoint  string
	statsFile   string
	dataset     []*jsonutil.Object
	batches     [][]indexedEntry
	// Every registered tool name sorted, the schema of the per row tool statistics
	allTools []string
	known    map[string]bool
	agents   agentRegistry
}

// Prints one formatted piece of run output in a single write
func (r *Runner) printf(format string, args ...any) {
	fmt.Fprintf(r.out, format, args...)
}

// Prints a title between two rules
func (r *Runner) banner(title string) {
	rule := strings.Repeat("=", 70)
	r.printf("\n%s\n%s\n%s\n", rule, title, rule)
}

// Joins a directory and a name the way a path object does, an absolute name replacing the directory
func joinPath(dir, name string) string {
	if filepath.IsAbs(name) {
		return filepath.Clean(name)
	}
	return filepath.Clean(dir + string(filepath.Separator) + name)
}

// Lines of a text read with universal newlines, the last one kept even without a line end
func splitLines(text string) []string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Text of a UTF-8 file, the decode failure reported with the offset of its first bad byte
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		offset := 0
		for offset < len(data) {
			r, size := utf8.DecodeRune(data[offset:])
			if r == utf8.RuneError && size <= 1 {
				break
			}
			offset += size
		}
		return "", fmt.Errorf("'utf-8' codec can't decode byte 0x%02x in position %d", data[min(offset, len(data)-1)], offset)
	}
	return string(data), nil
}

// New validates the distribution, creates the output directory, loads the dataset and splits it into batches
func New(ctx context.Context, o Options, out io.Writer) (*Runner, error) {
	if out == nil || out == io.Writer(os.Stdout) {
		out = termutil.Stdout
	}
	r := &Runner{o: o, out: out, home: home.Dir(ctx), datasetFile: filepath.Clean(o.DatasetFile)}
	r.cwd, _ = os.Getwd()
	if !tools.ValidateDistribution(o.Distribution) {
		return nil, fmt.Errorf("Unknown distribution: %s. Available: %s", o.Distribution, anyutil.Quote(availableDistributions()))
	}
	if o.BatchSize < 1 {
		return nil, errors.New("the batch size must be a positive integer")
	}
	r.outputDir = joinPath("data", o.RunName)
	if err := os.MkdirAll(r.outputDir, 0o777); err != nil {
		return nil, err
	}
	r.checkpoint = filepath.Join(r.outputDir, "checkpoint.json")
	r.statsFile = filepath.Join(r.outputDir, "statistics.json")
	if err := r.loadDataset(); err != nil {
		return nil, err
	}
	if o.MaxSamples != 0 && o.MaxSamples < len(r.dataset) {
		full := len(r.dataset)
		r.dataset = r.dataset[:max(sliceEnd(o.MaxSamples, full), 0)]
		r.printf("✂️  Truncated dataset from %d to %d samples (--max_samples)\n", full, o.MaxSamples)
	}
	r.batches = chunk(enumerate(r.dataset), o.BatchSize)
	r.printf("📊 Batch Runner Initialized\n")
	r.printf("   Dataset: %s (%d prompts)\n", r.datasetFile, len(r.dataset))
	r.printf("   Batch size: %d\n", o.BatchSize)
	r.printf("   Total batches: %d\n", len(r.batches))
	r.printf("   Run name: %s\n", o.RunName)
	r.printf("   Distribution: %s\n", o.Distribution)
	r.printf("   Output directory: %s\n", r.outputDir)
	r.printf("   Workers: %d\n", o.NumWorkers)
	if o.EphemeralSystemPrompt != "" {
		preview := o.EphemeralSystemPrompt
		if utf8.RuneCountInString(preview) > 60 {
			preview = textutil.First(preview, 60) + "..."
		}
		r.printf("   🔒 Ephemeral system prompt: '%s'\n", preview)
	}
	return r, nil
}

// End of a slice that keeps the first n items, a negative n counting back from the end
func sliceEnd(n, length int) int {
	if n < 0 {
		return length + n
	}
	return min(n, length)
}

// Distribution names in declaration order, the ones the distribution table lists
func availableDistributions() []string {
	listed := tools.ListDistributions()
	names := make([]string, 0, len(listed))
	for _, name := range tools.DistributionNames() {
		if _, ok := listed[name]; ok {
			names = append(names, name)
		}
	}
	return names
}

// Loads the JSONL entries that are objects with a prompt field, blank and invalid lines skipped with a warning
func (r *Runner) loadDataset() error {
	if _, err := os.Stat(r.datasetFile); err != nil {
		return fmt.Errorf("Dataset file not found: %s", r.datasetFile)
	}
	text, err := readText(r.datasetFile)
	if err != nil {
		return err
	}
	for n, line := range splitLines(text) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		decoded, err := jsonutil.Decode(line)
		if err != nil {
			r.printf("⚠️  Warning: Invalid JSON on line %d: %s\n", n+1, err.Error())
			continue
		}
		entry, ok := decoded.(*jsonutil.Object)
		if ok = ok && entry != nil; ok {
			_, ok = entry.Get("prompt")
		}
		if !ok {
			r.printf("⚠️  Warning: Line %d missing 'prompt' field, skipping\n", n+1)
			continue
		}
		r.dataset = append(r.dataset, entry)
	}
	if len(r.dataset) == 0 {
		return fmt.Errorf("No valid entries found in dataset file: %s", r.datasetFile)
	}
	return nil
}

// Dataset entries paired with their index
func enumerate(dataset []*jsonutil.Object) []indexedEntry {
	entries := make([]indexedEntry, len(dataset))
	for i, entry := range dataset {
		entries[i] = indexedEntry{index: i, entry: entry}
	}
	return entries
}

// Splits indexed entries into batches of size, each entry keeping its dataset index
func chunk(entries []indexedEntry, size int) [][]indexedEntry {
	var batches [][]indexedEntry
	for start := 0; start < len(entries); start += size {
		batches = append(batches, entries[start:min(start+size, len(entries))])
	}
	return batches
}

// Checkpoint of a run that recorded nothing yet
func (r *Runner) emptyCheckpoint() *jsonutil.Object {
	return jsonutil.ObjectOf("run_name", r.o.RunName, "completed_prompts", []any{}, "batch_stats", jsonutil.NewObject(), "last_updated", nil)
}

// Checkpoint on disk, an empty one when the file is missing or unreadable
func (r *Runner) loadCheckpoint() (*jsonutil.Object, error) {
	if _, err := os.Stat(r.checkpoint); err != nil {
		return r.emptyCheckpoint(), nil
	}
	text, err := readText(r.checkpoint)
	var decoded any
	if err == nil {
		decoded, err = jsonutil.Decode(text)
	}
	if err != nil {
		r.printf("⚠️  Warning: Failed to load checkpoint: %s\n", err.Error())
		return r.emptyCheckpoint(), nil
	}
	checkpoint, ok := decoded.(*jsonutil.Object)
	if !ok || checkpoint == nil {
		return nil, fmt.Errorf("the checkpoint %s holds %s, expected an object", r.checkpoint, anyutil.Kind(decoded))
	}
	return checkpoint, nil
}

// Stamps the checkpoint with the current time and replaces the file atomically
func (r *Runner) saveCheckpoint(ctx context.Context, checkpoint *jsonutil.Object) error {
	checkpoint.Set("last_updated", time.Now().Format(timeutil.RFC3339Micro))
	return config.AtomicJSONWrite(ctx, r.checkpoint, checkpoint)
}

// Prompt indices a checkpoint lists as completed
func completedIndices(checkpoint *jsonutil.Object) map[int]bool {
	completed := map[int]bool{}
	listed, _ := checkpoint.Get("completed_prompts")
	for _, item := range listItems(listed) {
		switch v := item.(type) {
		case int64:
			completed[int(v)] = true
		case int:
			completed[v] = true
		case float64:
			if v == float64(int(v)) {
				completed[int(v)] = true
			}
		}
	}
	return completed
}

// Completed prompt indices in ascending order as a JSON list
func sortedIndices(completed map[int]bool) []any {
	indices := make([]int, 0, len(completed))
	for index := range completed {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	out := make([]any, len(indices))
	for i, index := range indices {
		out[i] = index
	}
	return out
}

// Records one finished batch in the checkpoint and writes it, so a resume after a crash keeps the progress
func (r *Runner) checkpointBatch(ctx context.Context, checkpoint *jsonutil.Object, completed map[int]bool, result batchResult) error {
	for _, index := range result.completedPrompts {
		completed[index] = true
	}
	raw, present := checkpoint.Get("batch_stats")
	stats, ok := raw.(*jsonutil.Object)
	if !present {
		stats, ok = jsonutil.NewObject(), true
		checkpoint.Set("batch_stats", stats)
	}
	if !ok || stats == nil {
		return fmt.Errorf("the checkpoint batch_stats are %s, expected an object", anyutil.Kind(raw))
	}
	stats.Set(fmt.Sprint(result.batchNum), jsonutil.ObjectOf("processed", result.processed, "skipped", result.skipped, "discarded_no_reasoning", result.discarded))
	checkpoint.Set("completed_prompts", sortedIndices(completed))
	return r.saveCheckpoint(ctx, checkpoint)
}

// Sorted paths of the batch files of the run
func (r *Runner) batchFiles() []string {
	files, _ := filepath.Glob(filepath.Join(r.outputDir, "batch_*.jsonl"))
	sort.Strings(files)
	return files
}

// Prompt texts already processed, read from every batch file of the run so resume works even when indices moved
func (r *Runner) scanCompletedPrompts() map[string]bool {
	completed := map[string]bool{}
	files := r.batchFiles()
	if len(files) == 0 {
		return completed
	}
	r.printf("📂 Scanning %d batch files for completed prompts...\n", len(files))
	for _, file := range files {
		text, err := readText(file)
		if err != nil {
			r.printf("  ⚠️  Warning: Error reading %s: %s\n", filepath.Base(file), err.Error())
			continue
		}
		for _, line := range splitLines(text) {
			decoded, err := jsonutil.Decode(strings.TrimSpace(line))
			if err != nil {
				continue
			}
			fields, ok := jsonutil.AsMap(decoded)
			if !ok || anyutil.IsTrue(fields["failed"]) {
				continue
			}
			if text := entryPromptText(decoded); text != "" {
				completed[text] = true
			}
		}
	}
	return completed
}

// Stripped text of a dataset value that must be text
func mustStrip(v any) (string, error) {
	text, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("the dataset prompt is %s, expected a string", anyutil.Kind(v))
	}
	return strings.TrimSpace(text), nil
}

// Human prompt text of a dataset entry as resume matches it, the flat prompt or the first user turn of its conversations
func datasetPromptText(entry *jsonutil.Object) (string, error) {
	var prompt any = ""
	if v, ok := entry.Get("prompt"); ok {
		prompt = v
	}
	text, err := mustStrip(prompt)
	if err != nil || text != "" {
		return text, err
	}
	conversations, _ := entry.Get("conversations")
	for _, item := range listItems(conversations) {
		msg, ok := jsonutil.AsMap(item)
		if !ok {
			return "", fmt.Errorf("a dataset conversation turn is %s, expected an object", anyutil.Kind(item))
		}
		role := msg["role"]
		if !anyutil.IsTrue(role) {
			role = msg["from"]
		}
		if role != "user" && role != "human" {
			continue
		}
		content := msg["content"]
		if !anyutil.IsTrue(content) {
			content = ""
			if v, ok := msg["value"]; ok {
				content = v
			}
		}
		return mustStrip(content)
	}
	return "", nil
}

// Dataset entries not yet completed and the indices of the ones that are, a saved row matching in its redacted form too
func (r *Runner) filterCompleted(completed map[string]bool) ([]indexedEntry, []int, error) {
	var remaining []indexedEntry
	var skipped []int
	for index, entry := range r.dataset {
		text, err := datasetPromptText(entry)
		if err != nil {
			return nil, nil, err
		}
		redacted, _ := agent.RedactTrajectoryValue(text).(string)
		if completed[text] || completed[redacted] {
			skipped = append(skipped, index)
		} else {
			remaining = append(remaining, indexedEntry{index: index, entry: entry})
		}
	}
	return remaining, skipped, nil
}

// Rebuilds the batches from the prompts no batch file holds yet, false when nothing is left to run
func (r *Runner) applyResume() (bool, error) {
	completed := r.scanCompletedPrompts()
	if len(completed) == 0 {
		return true, nil
	}
	r.printf("   Found %d already-completed prompts by content matching\n", len(completed))
	remaining, skipped, err := r.filterCompleted(completed)
	if err != nil {
		return false, err
	}
	if len(remaining) == 0 {
		r.printf("\n✅ All prompts have already been processed!\n")
		return false, nil
	}
	r.batches = chunk(remaining, r.o.BatchSize)
	r.banner("📊 RESUME SUMMARY")
	r.printf("   Original dataset size:     %s prompts\n", textutil.Thousands(int64(len(r.dataset))))
	r.printf("   Already completed:         %s prompts\n", textutil.Thousands(int64(len(skipped))))
	r.printf("   ─────────────────────────────────────────\n")
	r.printf("   🎯 RESUMING WITH:          %s prompts\n", textutil.Thousands(int64(len(remaining))))
	r.printf("   New batches created:       %d\n", len(r.batches))
	r.printf("%s\n\n", strings.Repeat("=", 70))
	return true, nil
}

// Registers every tool the run can meet, plugin tools included, as the schema of the per row statistics
func (r *Runner) loadToolNames(ctx context.Context) {
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				pkgLog().Debug(fmt.Sprintf("Plugin discovery failed: %v", recovered))
			}
		}()
		plugins.DiscoverPlugins(ctx, false)
	}()
	r.allTools = append([]string{}, tools.Default.AllToolNames(ctx, "")...)
	sort.Strings(r.allTools)
	r.known = make(map[string]bool, len(r.allTools))
	for _, name := range r.allTools {
		r.known[name] = true
	}
}

// Run processes every batch, then merges the batch files and writes the statistics, resume first dropping prompts a batch file already holds
func (r *Runner) Run(ctx context.Context, resume bool) error {
	r.banner("🚀 Starting Batch Processing")
	if resume {
		proceed, err := r.applyResume()
		if err != nil || !proceed {
			return err
		}
	}
	checkpoint, err := r.loadCheckpoint()
	if err != nil {
		return err
	}
	if name, _ := checkpoint.Get("run_name"); name != r.o.RunName {
		checkpoint = r.emptyCheckpoint()
	}
	r.loadToolNames(ctx)
	completed := completedIndices(checkpoint)
	started := time.Now()
	results, err := r.runPool(ctx, checkpoint, completed)
	if err != nil {
		return err
	}
	totalTools, totalReasoning, discarded, processed := newToolStats(), reasoningStats{}, 0, 0
	for _, result := range results {
		totalTools.merge(result.toolStats)
		totalReasoning.merge(result.reasoning)
		discarded += result.discarded
		processed += result.processed
	}
	checkpoint.Set("completed_prompts", sortedIndices(completed))
	if err := r.saveCheckpoint(ctx, checkpoint); err != nil {
		r.printf("⚠️  Warning: Failed to save final checkpoint: %s\n", err.Error())
	}
	kept, files, err := r.combineBatchFiles()
	if err != nil {
		return err
	}
	stats := jsonutil.ObjectOf(
		"run_name", r.o.RunName, "distribution", r.o.Distribution, "total_prompts", len(r.dataset), "total_batches", len(r.batches),
		"batch_size", r.o.BatchSize, "model", r.o.Model, "completed_at", time.Now().Format(timeutil.RFC3339Micro),
		"duration_seconds", roundTo(time.Since(started).Seconds(), 2), "tool_statistics", totalTools.statistics(),
		"reasoning_statistics", totalReasoning.object(), "discarded_no_reasoning", discarded,
	)
	text, err := jsonutil.Encode(stats, jsonutil.Indent(2))
	if err != nil {
		return err
	}
	if err := os.WriteFile(r.statsFile, []byte(text), 0o666); err != nil {
		return err
	}
	r.printSummary(processed, kept, files, discarded, totalTools, totalReasoning, started)
	return nil
}
