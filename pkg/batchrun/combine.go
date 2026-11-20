package batchrun

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Batch number in a batch file name, for the lines that name a filtered entry
func batchNumberOf(file string) string {
	stem := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	if parts := strings.Split(stem, "_"); len(parts) > 1 {
		return parts[1]
	}
	return stem
}

// First tool of a row's statistics that no registered tool matches, empty when every tool is known
func (r *Runner) unknownTool(row map[string]any) string {
	var names []string
	switch stats := row["tool_stats"].(type) {
	case *pyjson.Object:
		if stats != nil {
			names = stats.Keys
		}
	case map[string]any:
		for name := range stats {
			names = append(names, name)
		}
	}
	for _, name := range names {
		if !r.known[name] {
			return name
		}
	}
	return ""
}

// Merges every batch file of the run, old and resumed, into trajectories.jsonl and returns the rows kept and the files read
func (r *Runner) combineBatchFiles() (kept, found int, err error) {
	combined := filepath.Join(r.outputDir, "trajectories.jsonl")
	r.printf("\n📦 Combining ALL batch files into %s...\n", filepath.Base(combined))
	out, err := os.Create(combined)
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
	}()
	writer := bufio.NewWriter(out)
	total, filtered, tombstones := 0, 0, 0
	for _, file := range r.batchFiles() {
		found++
		batchNum := batchNumberOf(file)
		text, err := readText(file)
		if err != nil {
			return 0, found, err
		}
		for _, line := range splitLines(text) {
			total++
			decoded, err := pyjson.Loads(line)
			if err != nil {
				filtered++
				r.printf("   ⚠️  Filtering invalid JSON entry (batch %s)\n", batchNum)
				continue
			}
			row, ok := pyjson.Dict(decoded)
			if !ok {
				filtered++
				continue
			}
			if pyrepr.Truthy(row["discarded"]) {
				tombstones++
				continue
			}
			if invalid := r.unknownTool(row); invalid != "" {
				filtered++
				if pystr.Len(invalid) > 50 {
					invalid = pystr.Slice(invalid, 0, 50) + "..."
				}
				r.printf("   ⚠️  Filtering corrupted entry (batch %s): invalid tool '%s'\n", batchNum, invalid)
				continue
			}
			if _, err := writer.WriteString(line + "\n"); err != nil {
				return 0, found, err
			}
		}
	}
	if err := writer.Flush(); err != nil {
		return 0, found, err
	}
	if filtered > 0 {
		r.printf("⚠️  Filtered %d corrupted entries out of %d total\n", filtered, total)
	}
	kept = total - filtered - tombstones
	r.printf("✅ Combined %d batch files into trajectories.jsonl (%d entries)\n", found, kept)
	return kept, found, nil
}

// Text padded on the right to width characters, longer text left as it is
func padRight(text string, width int) string {
	if n := pystr.Len(text); n < width {
		return text + strings.Repeat(" ", width-n)
	}
	return text
}

// Share of part in total as a percentage rounded to one decimal place
func percentOf(part, total int) string {
	return pyrepr.Float(roundTo(float64(part)/float64(total)*100, 1))
}

// Prints what the run processed, the tool usage table, the reasoning coverage and where the results are
func (r *Runner) printSummary(processed, kept, files, discarded int, usage *toolStats, reasoning reasoningStats, started time.Time) {
	rule := strings.Repeat("-", 70)
	r.banner("📊 BATCH PROCESSING COMPLETE")
	r.printf("✅ Prompts processed this run: %d\n", processed)
	r.printf("✅ Total trajectories in merged file: %d\n", kept)
	r.printf("✅ Total batch files merged: %d\n", files)
	r.printf("⏱️  Total duration: %ss\n", pyrepr.Float(roundTo(time.Since(started).Seconds(), 2)))
	r.printf("\n📈 Tool Usage Statistics:\n%s\n", rule)
	if len(usage.names) > 0 {
		r.printf("%s %s %s %s %s\n", padRight("Tool Name", 25), padRight("Count", 10), padRight("Success", 10), padRight("Failure", 10), padRight("Success Rate", 12))
		r.printf("%s\n", rule)
		for _, name := range usage.byCount() {
			stat := usage.byTool[name]
			success, _ := stat.rates()
			r.printf("%s %s %s %s %.1f%%\n", padRight(name, 25), padRight(fmt.Sprint(stat.count), 10), padRight(fmt.Sprint(stat.success), 10),
				padRight(fmt.Sprint(stat.failure), 10), success)
		}
	} else {
		r.printf("No tool calls were made during this run.\n")
	}
	r.printf("\n🧠 Reasoning Coverage:\n%s\n", rule)
	if reasoning.total > 0 {
		r.printf("   Total assistant turns:    %s\n", pystr.GroupThousands(int64(reasoning.total)))
		r.printf("   With reasoning:           %s (%s%%)\n", pystr.GroupThousands(int64(reasoning.with)), percentOf(reasoning.with, reasoning.total))
		r.printf("   Without reasoning:        %s (%s%%)\n", pystr.GroupThousands(int64(reasoning.without)), percentOf(reasoning.without, reasoning.total))
	} else {
		r.printf("   No assistant turns recorded.\n")
	}
	if discarded > 0 {
		r.printf("   🚫 Samples discarded (zero reasoning): %s\n", pystr.GroupThousands(int64(discarded)))
	}
	r.printf("\n💾 Results saved to: %s\n", r.outputDir)
	r.printf("   - Trajectories: trajectories.jsonl (combined)\n")
	r.printf("   - Individual batches: batch_*.jsonl (for debugging)\n")
	r.printf("   - Statistics: %s\n", filepath.Base(r.statsFile))
	r.printf("   - Checkpoint: %s\n", filepath.Base(r.checkpoint))
}
