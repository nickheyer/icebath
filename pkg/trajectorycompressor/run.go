package trajectorycompressor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// One parsed JSONL line and its number
type jsonlEntry struct {
	line  int
	entry any
}

// UnicodeDecodeError text for the first byte of data that is not UTF-8
func utf8Error(data []byte) error {
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size > 1 {
			i += size
			continue
		}
		b := data[i]
		reason := "invalid continuation byte"
		switch {
		case b < 0xC2 || b > 0xF4:
			reason = "invalid start byte"
		case !utf8.FullRune(data[i:]):
			reason = "unexpected end of data"
		}
		return &pyint.Error{Kind: "UnicodeDecodeError", Msg: fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: %s", b, i, reason)}
	}
	return nil
}

// Lines of text read in universal newline mode, each keeping its newline
func universalLines(text string) []string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var lines []string
	for text != "" {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			lines = append(lines, text)
			break
		}
		lines = append(lines, text[:i+1])
		text = text[i+1:]
	}
	return lines
}

// Entries of every non blank line numbered from start, undecodable lines passed to onError
func loadJSONL(path string, onError func(int, error), start int) ([]jsonlEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := utf8Error(data); err != nil {
		return nil, err
	}
	var entries []jsonlEntry
	for k, line := range universalLines(string(data)) {
		n := start + k
		if pystr.Strip(line) == "" {
			continue
		}
		v, err := pyjson.Loads(line)
		if err != nil {
			var de *pyjson.DecodeError
			if !errors.As(err, &de) {
				return nil, err
			}
			if onError != nil {
				onError(n, err)
			}
			continue
		}
		entries = append(entries, jsonlEntry{n, v})
	}
	return entries, nil
}

// Writes one ensure_ascii=False JSON document per line
func writeJSONL(path string, entries []any) error {
	var b strings.Builder
	for _, e := range entries {
		s, err := pyjson.Dumps(e, pyjson.EnsureASCII(false))
		if err != nil {
			return err
		}
		b.WriteString(s + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o666)
}

// str of a pathlib path built from a directory and a child name
func joinPath(dir, name string) string {
	return pypath.PurePathStr(dir + string(filepath.Separator) + name)
}

// Sorted paths of the *.jsonl entries of a directory, none when it cannot be listed
func globJSONL(dir string) []string {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, it := range items {
		if ok, _ := filepath.Match("*.jsonl", it.Name()); ok {
			out = append(out, joinPath(dir, it.Name()))
		}
	}
	sort.Strings(out)
	return out
}

const statusFormat = "[dim]✅ %d compressed | ⏭️ %d skipped | ⏱️ %d timeout | 🔄 %d API calls | ⚡ %d in-flight[/dim]"

// Shared counters and progress handles of one directory run
type runProgress struct {
	progress   *pyrich.Progress
	mainTask   int
	statusTask int
	mu         sync.Mutex
	sem        chan struct{}
	compressed int
	skipped    int
	apiCalls   int
	inFlight   int
	timeouts   int
}

// Retires one in flight entry and advances the bar, the caller holding the lock
func (r *runProgress) finish(updateStatus bool) {
	r.inFlight--
	r.progress.Advance(r.mainTask, 1)
	if updateStatus {
		r.progress.Update(r.statusTask, fmt.Sprintf(statusFormat, r.compressed, r.skipped, r.timeouts, r.apiCalls, r.inFlight))
	}
}

// Outcome of one entry, dropped when it timed out
type entryOutcome struct {
	entry   any
	dropped bool
}

// Processes one entry under the semaphore and timeout, the original entry kept on error
func (c *Compressor) processOne(ctx context.Context, run *runProgress, filePath string, entryIdx int, entry any) entryOutcome {
	run.sem <- struct{}{}
	defer func() { <-run.sem }()
	run.mu.Lock()
	run.inFlight++
	run.mu.Unlock()
	timeout := time.Duration(c.Config.PerTrajectoryTimeout) * time.Second
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type result struct {
		entry any
		m     *TrajectoryMetrics
		err   error
	}
	var res result
	timedOut := timeout <= 0
	if !timedOut {
		done := make(chan result, 1)
		go func() {
			e, m, err := c.ProcessEntry(tctx, entry)
			done <- result{e, m, err}
		}()
		select {
		case res = <-done:
			timedOut = res.err != nil && tctx.Err() == context.DeadlineExceeded
		case <-tctx.Done():
			timedOut = true
		}
	}
	if timedOut {
		c.logger.Warning(fmt.Sprintf("Timeout processing entry from %s:%d (>%ds)", filePath, entryIdx, c.Config.PerTrajectoryTimeout))
		run.mu.Lock()
		c.Aggregate.TrajectoriesFailed++
		run.timeouts++
		run.finish(true)
		run.mu.Unlock()
		return entryOutcome{dropped: true}
	}
	if res.err != nil {
		c.logger.Error(fmt.Sprintf("Error processing entry from %s:%d: %s", filePath, entryIdx, res.err))
		run.mu.Lock()
		c.Aggregate.TrajectoriesFailed++
		run.finish(false)
		run.mu.Unlock()
		return entryOutcome{entry: entry}
	}
	run.mu.Lock()
	c.Aggregate.AddTrajectoryMetrics(res.m)
	if res.m.WasCompressed {
		run.compressed++
		run.apiCalls += res.m.SummarizationAPICalls
	}
	if res.m.SkippedUnderTarget {
		run.skipped++
	}
	run.finish(true)
	run.mu.Unlock()
	return entryOutcome{entry: res.entry}
}

// Compresses every *.jsonl of inputDir into outputDir with concurrent summary calls, then reports and saves metrics
func (c *Compressor) ProcessDirectory(ctx context.Context, inputDir, outputDir string) error {
	console := pyrich.NewConsole(os.Stdout)
	c.Aggregate.ProcessingStartTime = pydatetime.ISOFormatNaive(time.Now())
	started := time.Now()
	inputDir, outputDir = pypath.PurePathStr(inputDir), pypath.PurePathStr(outputDir)
	files := globJSONL(inputDir)
	if len(files) == 0 {
		c.logger.Warning(fmt.Sprintf("No JSONL files found in %s", inputDir))
		return nil
	}
	console.Print("\n[dim]Loading all entries...[/dim]")
	type item struct {
		file  string
		idx   int
		entry any
	}
	var all []item
	for _, f := range files {
		warn := func(line int, e error) {
			c.logger.Warning(fmt.Sprintf("Skipping invalid JSON at %s:%d: %s", f, line, e))
		}
		entries, err := loadJSONL(f, warn, 0)
		if err != nil {
			return err
		}
		for _, e := range entries {
			all = append(all, item{f, e.line, e.entry})
		}
	}
	total := len(all)
	rule := strings.Repeat("=", 60)
	console.Print("\n" + rule)
	console.Print("📂 Input: " + inputDir)
	console.Print("📂 Output: " + outputDir)
	console.Print(fmt.Sprintf("📄 Files to process: %d", len(files)))
	console.Print("📊 Total trajectories: " + pystr.GroupThousands(int64(total)))
	console.Print("🎯 Target max tokens: " + pystr.GroupThousands(int64(c.Config.TargetMaxTokens)))
	console.Print(fmt.Sprintf("📝 Summary target tokens: %d", c.Config.SummaryTargetTokens))
	console.Print(fmt.Sprintf("⚡ Max concurrent API calls: %d", c.Config.MaxConcurrentRequests))
	console.Print(rule + "\n")

	progress := console.NewProgress(10, &pyrich.SpinnerColumn{}, pyrich.TextColumn{Format: "[progress.description]{task.description}"},
		pyrich.BarColumn{}, pyrich.TaskProgressColumn{}, pyrich.TextColumn{Format: "•"}, pyrich.TimeElapsedColumn{},
		pyrich.TextColumn{Format: "•"}, pyrich.TimeRemainingColumn{})
	progress.Start()
	totalF := float64(total)
	run := &runProgress{progress: progress}
	run.mainTask = progress.AddTask("[cyan]Compressing "+pystr.GroupThousands(int64(total))+" trajectories", &totalF)
	run.statusTask = progress.AddTask("[dim]Starting...[/dim]", nil)
	if c.Config.MaxConcurrentRequests < 0 {
		progress.Stop()
		return &pyint.Error{Kind: "ValueError", Msg: "Semaphore initial value must be >= 0"}
	}
	run.sem = make(chan struct{}, c.Config.MaxConcurrentRequests)
	outcomes := make([]entryOutcome, total)
	var wg sync.WaitGroup
	for i, it := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcomes[i] = c.processOne(ctx, run, it.file, it.idx, it.entry)
		}()
	}
	wg.Wait()
	progress.RemoveTask(run.statusTask)
	progress.Stop()

	console.Print("\n[dim]Writing output files...[/dim]")
	if err := os.MkdirAll(outputDir, 0o777); err != nil {
		return err
	}
	results := map[string][]any{}
	for i, it := range all {
		if !outcomes[i].dropped {
			results[it.file] = append(results[it.file], outcomes[i].entry)
		}
	}
	for _, f := range files {
		if err := writeJSONL(joinPath(outputDir, filepath.Base(f)), results[f]); err != nil {
			return err
		}
	}
	c.Aggregate.ProcessingEndTime = pydatetime.ISOFormatNaive(time.Now())
	c.Aggregate.ProcessingDurationSeconds = time.Since(started).Seconds()
	c.printSummary()
	if c.Config.MetricsEnabled {
		metricsPath := joinPath(outputDir, c.Config.MetricsOutputFile)
		text, err := pyjson.Dumps(c.Aggregate.ToDict(), pyjson.Indent(2))
		if err != nil {
			return err
		}
		if err := os.WriteFile(metricsPath, []byte(text), 0o666); err != nil {
			return err
		}
		console.Print("\n💾 Metrics saved to " + metricsPath)
	}
	return nil
}

// Python format(value, spec) of a report value
func pf(v any, spec string) string {
	s, err := pyrepr.Format(v, spec)
	if err != nil {
		panic(err)
	}
	return s
}

// Value at a dotted section and key of an ordered metrics dict
func dictAt(d *pyjson.Object, section, key string) any {
	sec, _ := d.Get(section)
	v, _ := sec.(*pyjson.Object).Get(key)
	return v
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	panic("metrics value is not a number")
}

// Prints the boxed compression report and the ratio and savings distribution
func (c *Compressor) printSummary() {
	m := c.Aggregate.ToDict()
	s := func(k string) any { return dictAt(m, "summary", k) }
	t := func(k string) any { return dictAt(m, "tokens", k) }
	u := func(k string) any { return dictAt(m, "turns", k) }
	a := func(k string) any { return dictAt(m, "averages", k) }
	z := func(k string) any { return dictAt(m, "summarization", k) }
	p := func(k string) any { return dictAt(m, "processing", k) }
	total, compressed := s("total_trajectories").(int64), s("trajectories_compressed").(int64)
	pct := func(n any) float64 { return asFloat(n) / float64(max(total, 1)) * 100 }
	duration := asFloat(p("duration_seconds"))
	timeStr := pf(duration, ".1f") + " seconds"
	if duration > 60 {
		timeStr = pf(duration/60, ".1f") + " minutes"
	}
	pad := strings.Repeat
	row := func(body string) string { return "║    " + body + "║" }
	first19 := func(v any) string {
		r := []rune(v.(string))
		return string(r[:min(19, len(r))])
	}
	tokens := []string{
		row("Before Compression:     " + pf(t("total_before"), ">15,") + " tokens" + pad(" ", 21)),
		row("After Compression:      " + pf(t("total_after"), ">15,") + " tokens" + pad(" ", 21)),
		row("Total Saved:            " + pf(t("total_saved"), ">15,") + " tokens" + pad(" ", 21)),
		row("Overall Compression:    " + pf(t("overall_compression_ratio"), ">14.1%") + pad(" ", 28)),
	}
	if before := t("total_before").(int64); before > 0 {
		tokens = append(tokens, row("Space Savings:          "+pf(float64(t("total_saved").(int64))/float64(before)*100, ">14.1f")+"%"+pad(" ", 28)))
	}
	averages := []string{row("No trajectories were compressed" + pad(" ", 38))}
	if compressed > 0 {
		averages = []string{
			row("Avg Compression Ratio:  " + pf(a("avg_compression_ratio"), ">14.1%") + pad(" ", 28)),
			row("Avg Tokens Saved:       " + pf(a("avg_tokens_saved_per_compressed"), ">14,.0f") + pad(" ", 28)),
			row("Avg Turns Removed:      " + pf(a("avg_turns_removed_per_compressed"), ">14.1f") + pad(" ", 28)),
		}
	}
	sections := []struct {
		title string
		pad   int
		rows  []string
	}{
		{"📁 TRAJECTORIES", 54, []string{
			row("Total Processed:        " + pf(total, ">10,") + pad(" ", 32)),
			row("├─ Compressed:          " + pf(compressed, ">10,") + "  (" + pf(pct(compressed), ">5.1f") + "%)" + pad(" ", 18)),
			row("├─ Skipped (under limit):" + pf(s("trajectories_skipped_under_target"), ">9,") + "  (" + pf(pct(s("trajectories_skipped_under_target")), ">5.1f") + "%)" + pad(" ", 18)),
			row("├─ Still over limit:    " + pf(s("trajectories_still_over_limit"), ">10,") + "  (" + pf(pct(s("trajectories_still_over_limit")), ">5.1f") + "%)" + pad(" ", 18)),
			row("└─ Failed:              " + pf(s("trajectories_failed"), ">10,") + pad(" ", 32)),
		}},
		{"🔢 TOKENS", 60, tokens},
		{"💬 CONVERSATION TURNS", 48, []string{
			row("Before Compression:     " + pf(u("total_before"), ">15,") + " turns" + pad(" ", 22)),
			row("After Compression:      " + pf(u("total_after"), ">15,") + " turns" + pad(" ", 22)),
			row("Total Removed:          " + pf(u("total_removed"), ">15,") + " turns" + pad(" ", 22)),
		}},
		{"📈 AVERAGES (Compressed Trajectories Only)", 27, averages},
		{"🤖 SUMMARIZATION API", 49, []string{
			row("API Calls Made:         " + pf(z("total_api_calls"), ">15,") + pad(" ", 27)),
			row("Errors:                 " + pf(z("total_errors"), ">15,") + pad(" ", 27)),
			row("Success Rate:           " + pf(z("success_rate"), ">14.1%") + pad(" ", 28)),
		}},
		{"⏱️  PROCESSING TIME", 51, []string{
			row("Duration:               " + pf(timeStr, ">20") + pad(" ", 22)),
			row("Throughput:             " + pf(float64(total)/max(duration, 0.001), ">15.1f") + " traj/sec" + pad(" ", 18)),
			row("Started:                " + pf(first19(p("start_time")), ">20") + pad(" ", 22)),
			row("Finished:               " + pf(first19(p("end_time")), ">20") + pad(" ", 22)),
		}},
	}
	fmt.Print("\n\n")
	fmt.Println("╔" + pad("═", 70) + "╗")
	fmt.Println("║" + pf("TRAJECTORY COMPRESSION REPORT", "^70") + "║")
	for _, sec := range sections {
		fmt.Println("╠" + pad("═", 70) + "╣")
		fmt.Println("║  " + sec.title + pad(" ", sec.pad) + "║")
		fmt.Println("║" + pad("─", 70) + "║")
		for _, r := range sec.rows {
			fmt.Println(r)
		}
	}
	fmt.Println("╚" + pad("═", 70) + "╝")

	ratios := c.Aggregate.CompressionRatios
	if len(ratios) == 0 {
		return
	}
	saved := c.Aggregate.TokensSavedList
	sr := append([]float64(nil), ratios...)
	sort.Float64s(sr)
	ss := append([]int(nil), saved...)
	sort.Ints(ss)
	fmt.Println("\n📊 Distribution Summary:")
	fmt.Printf("   Compression ratios: min=%s, max=%s, median=%s\n", pf(sr[0], ".2%"), pf(sr[len(sr)-1], ".2%"), pf(sr[len(sr)/2], ".2%"))
	fmt.Printf("   Tokens saved:       min=%s, max=%s, median=%s\n", pf(int64(ss[0]), ","), pf(int64(ss[len(ss)-1]), ","), pf(int64(ss[len(ss)/2]), ","))
}
