package hostinfo

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	smiTTL     = 4 * time.Second
	smiTimeout = 10 * time.Second
	smiQuery   = "--query-gpu=memory.total,memory.free,name,pci.device_id,memory.used,utilization.gpu"
	smiFields  = 6
)

// First GPU row nvidia-smi reports, PCIID nil when the device reports no identity
type SMIReading struct {
	Name               string
	TotalBytes         uint64
	FreeBytes          uint64
	UsedBytes          uint64
	UtilizationPercent int
	PCIID              *uint32
}

// First GPU nvidia-smi reports, kept four seconds failures included, ErrAbsent when nvidia-smi is not installed
func (h *Host) NvidiaSMI(ctx context.Context) (SMIReading, error) {
	return h.smi.get(ctx, smiTTL, false, h.querySMI)
}

// Absolute nvidia-smi path from PATH or the driver install locations, read once
func (h *Host) smiExecutable() (string, error) {
	return h.smiPath.get(func() (string, error) {
		if path, err := exec.LookPath("nvidia-smi"); err == nil {
			return path, nil
		}
		for _, candidate := range smiCandidates() {
			if exists(candidate) {
				return candidate, nil
			}
		}
		return "", fmt.Errorf("nvidia-smi: %w", ErrAbsent)
	})
}

// One nvidia-smi query under a ten second deadline, its first CSV row parsed
func (h *Host) querySMI(ctx context.Context) (SMIReading, error) {
	path, err := h.smiExecutable()
	if err != nil {
		return SMIReading{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, smiTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, smiQuery, "--format=csv,noheader,nounits")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	hideConsole(cmd)
	if err := cmd.Run(); err != nil {
		return SMIReading{}, fmt.Errorf("nvidia-smi: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	text := strings.TrimSpace(stdout.String())
	if text == "" {
		return SMIReading{}, errors.New("nvidia-smi: no GPU rows")
	}
	reader := csv.NewReader(strings.NewReader(text))
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	row, err := reader.Read()
	if err != nil {
		return SMIReading{}, fmt.Errorf("nvidia-smi: %w", err)
	}
	return parseSMIRow(row)
}

// Reading of one CSV row of the query, an error when any memory or utilization field is not a number
func parseSMIRow(row []string) (SMIReading, error) {
	if len(row) != smiFields {
		return SMIReading{}, fmt.Errorf("nvidia-smi: row has %d fields, want %d", len(row), smiFields)
	}
	var numbers [4]uint64
	for i, field := range []string{row[0], row[1], row[4], row[5]} {
		n, err := strconv.ParseUint(strings.TrimSpace(field), 10, 64)
		if err != nil {
			return SMIReading{}, fmt.Errorf("nvidia-smi: %w", err)
		}
		numbers[i] = n
	}
	reading := SMIReading{Name: strings.TrimSpace(row[2]), TotalBytes: numbers[0] << 20, FreeBytes: numbers[1] << 20,
		UsedBytes: numbers[2] << 20, UtilizationPercent: int(numbers[3])}
	id := strings.TrimSpace(row[3])
	if len(id) > 2 && id[0] == '0' && (id[1] == 'x' || id[1] == 'X') {
		id = id[2:]
	}
	if n, err := strconv.ParseUint(id, 16, 32); err == nil {
		packed := uint32(n)
		reading.PCIID = &packed
	}
	return reading, nil
}
