package hostinfo

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// CPU model as the OS names it, read once, empty when the OS names none
func (h *Host) processorModel() string { return h.cpuModel.value(cpuModel) }

// CPU vendor as the OS names it, read once, empty when the OS names none
func (h *Host) processorVendor() string {
	return h.cpuVendor.value(func() string { return cpuVendor(h) })
}

// One, five and fifteen minute run queue averages, sampled by this Host on Windows from its first read
func (h *Host) LoadAverage() ([3]float64, error) { return h.load.read() }

// System wide CPU use in percent measured across interval, rounded to one decimal, zero when no CPU time elapsed
func CPUPercent(ctx context.Context, interval time.Duration) (float64, error) {
	before, err := cpuTimes()
	if err != nil {
		return 0, err
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-timer.C:
	}
	after, err := cpuTimes()
	if err != nil {
		return 0, err
	}
	deltas := make([]float64, len(before))
	for i := range before {
		deltas[i] = max(0, after[i]-before[i])
	}
	total, busy := cpuTotalBusy(deltas)
	if total == 0 {
		return 0, nil
	}
	return roundDecimals(busy/total*100, 1), nil
}

// Value rounded to the given decimals with exact halves going to the even digit
func roundDecimals(value float64, decimals int) float64 {
	rounded, _ := strconv.ParseFloat(strconv.FormatFloat(value, 'f', decimals, 64), 64)
	return rounded
}

// Up to limit bytes of a text file without its byte order mark, empty when unreadable
func readText(path string, limit int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)))
	if err != nil {
		return ""
	}
	text := strings.TrimPrefix(string(data), "\xEF\xBB\xBF")
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\uFFFD")
	}
	return text
}
