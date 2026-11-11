package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Statuses an admission backed Bot mailbox receipt may carry
var canonicalReceiptStatuses = map[string]bool{"canonical": true, "queued": true, "claimed": true, "ambiguous": true,
	"settled": true, "failed": true, "cancelled": true}

// Canonical fields every admission backed receipt names
var canonicalReceiptFields = []string{"admission_id", "profile_home", "session_id", "principal_id"}

// Canonical fields a pin carries before its admission exists
var canonicalPinFields = []string{"profile_home", "session_id", "principal_id"}

// File name of a path without its last suffix
func pathStem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// Why an admission backed receipt is unsafe for bulk recovery, empty when it is canonical
func canonicalRecordError(path string, record map[string]any, required []string) string {
	deliveryID, ok := record["delivery_id"].(string)
	if !ok || deliveryID != pathStem(path) {
		return "delivery id does not match filename"
	}
	if _, err := checkDeliveryID(deliveryID); err != nil {
		return "delivery id is invalid"
	}
	var missing []string
	for _, key := range required {
		if text, isText := record[key].(string); !isText || text == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return "missing canonical fields: " + strings.Join(missing, ", ")
	}
	if _, isText := record["message"].(string); !isText {
		return "message is not a string"
	}
	status, isText := record["status"].(string)
	if !isText || !canonicalReceiptStatuses[status] {
		return "unknown canonical status " + pyrepr.Repr(record["status"])
	}
	return ""
}

// Why a scanned receipt matches none of the canonical, refused or legacy shapes, empty when one fits
func recordShapeError(path string, record map[string]any) string {
	_, admitted := record["admission_id"]
	if record["refused"] == true && !admitted {
		if record["status"] != "failed" {
			return "refused receipt is not failed"
		}
		return canonicalRecordError(path, record, canonicalPinFields)
	}
	_, principal := record["principal_id"]
	if admitted || record["status"] == "canonical" || principal {
		return canonicalRecordError(path, record, canonicalReceiptFields)
	}
	if _, legacy := record["owner"]; legacy {
		return ticketShapeError(path, record)
	}
	return "unknown receipt schema"
}

// True for the ambiguous pin an admission writes before it admits, never refused
func isPreAdmissionPin(path string, record map[string]any) bool {
	_, admitted := record["admission_id"]
	_, refused := record["refused"]
	return record["status"] == "ambiguous" && !admitted && !refused && canonicalRecordError(path, record, canonicalPinFields) == ""
}

// Python str of a read failure, the OSError text for filesystem errors
func botScanErrorText(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}

// One receipt of a bulk scan, its pin collected for an owner scan, a problem text when it is skipped
func scanBotRecord(path string, pins *[]botMailboxPin) (map[string]any, bool, string) {
	record, err := readTicket(path)
	if err != nil {
		return nil, false, botScanErrorText(err)
	}
	if record == nil {
		return nil, false, ""
	}
	problem := recordShapeError(path, record)
	if problem != "" && pins != nil && isPreAdmissionPin(path, record) {
		info, err := os.Stat(path)
		if err != nil {
			return nil, false, botScanErrorText(err)
		}
		*pins = append(*pins, botMailboxPin{Path: path, Record: record, MTime: float64(info.ModTime().UnixNano()) / 1e9})
		return nil, true, ""
	}
	return record, false, problem
}

// Bulk mailbox scan skipping unreadable or malformed receipts with a warning, an owner scan collecting pre admission pins
func scanBotRecords(root string, pins *[]botMailboxPin) []botMailboxRecord {
	entries, _ := os.ReadDir(root)
	var records []botMailboxRecord
	for _, entry := range entries {
		name := entry.Name()
		if runtime.GOOS == "windows" {
			name = strings.ToLower(name)
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		record, pinned, problem := scanBotRecord(path, pins)
		if pinned {
			continue
		}
		if problem != "" {
			slog.Warn(fmt.Sprintf("Skipping malformed Bot mailbox receipt %s (%s)", path, problem))
			continue
		}
		if record != nil {
			records = append(records, botMailboxRecord{Path: path, Record: record})
		}
	}
	return records
}
