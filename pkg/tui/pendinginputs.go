package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"

	"github.com/nickheyer/icebath/pkg/home"
)

var pendingRecordName = regexp.MustCompile(`^[0-9a-f-]{36}\.json$`)

// Input waiting in the composer queue, with its durable journal identity
type queueItem struct {
	Display             string
	Text                string
	Queued              *bool
	CreatedAt           *float64
	SubmissionID        string
	Destination         *submissionDestination
	OwnerDestination    *submissionDestination
	InFlight            bool
	Failed              bool
	Attachments         []imageAttachment
	ControlMethod       string
	ExecutionGeneration *float64
	PreparedText        *string
	LegacyAttempted     *bool
	Settle              func(accepted bool)
}

// Reports whether the owner took the input, for a queue entry waiting on its answer
func (q *queueItem) settle(accepted bool) {
	if q.Settle != nil {
		q.Settle(accepted)
	}
}

// Journal folder of a profile home, the process home when the destination names none
func pendingDirectory(profileHome string) string {
	if profileHome == "" {
		profileHome = home.Process()
	}
	return filepath.Join(profileHome, "tui-pending-inputs")
}

// Key of a destination, its profile home, profile and session
func pendingDestinationKey(d submissionDestination) string {
	var sid any
	if d.Sid != "" {
		sid = d.Sid
	}
	raw, _ := json.Marshal([]any{d.ProfileHome, d.Profile, sid})
	return string(raw)
}

// Successor chains of migrated destinations and the revision queue readers reload on
type pendingInputs struct {
	successors map[string]submissionDestination
	revision   int
}

var pendingJournal = &pendingInputs{successors: map[string]submissionDestination{}}

// Destination that owns inputs queued for d now, following migrations
func (p *pendingInputs) owner(d submissionDestination) submissionDestination {
	current := d
	seen := map[string]bool{}
	for key := pendingDestinationKey(current); !seen[key]; key = pendingDestinationKey(current) {
		seen[key] = true
		next, has := p.successors[key]
		if !has {
			break
		}
		current = next
	}
	return current
}

// Moves the inputs journaled for a session to the session that replaced it
func (p *pendingInputs) migrate(previous submissionDestination, successorSid, successorStoredSid string) error {
	if previous.Sid == "" || successorSid == "" || (previous.Sid == successorSid && previous.StoredSid == successorStoredSid) {
		return nil
	}
	successor := previous
	successor.Sid, successor.StoredSid = successorSid, successorStoredSid
	items, err := p.load(previous, true)
	if err != nil {
		return err
	}
	for i := range items {
		item := &items[i]
		keyed := item.OwnerDestination
		if keyed == nil {
			keyed = item.Destination
		}
		p.successors[pendingDestinationKey(*keyed)] = successor
		item.OwnerDestination = &successor
		if !item.Failed {
			item.Destination = &successor
		}
		if err := p.save(*item); err != nil {
			return err
		}
	}
	p.successors[pendingDestinationKey(previous)] = successor
	p.revision++
	return nil
}

// Destination as the journal stores it, an empty session or stored id as null
func destinationJSON(d *submissionDestination) any {
	if d == nil {
		return nil
	}
	out := map[string]any{"sid": nil, "storedSid": nil, "profile": d.Profile, "profileHome": d.ProfileHome}
	if d.Sid != "" {
		out["sid"] = d.Sid
	}
	if d.StoredSid != "" {
		out["storedSid"] = d.StoredSid
	}
	if d.OwnerHome != "" {
		out["ownerHome"] = d.OwnerHome
	}
	return out
}

// Destination of a journal record, nil when the record holds none
func destinationOf(v any) *submissionDestination {
	m, isObj := v.(map[string]any)
	if !isObj {
		return nil
	}
	return &submissionDestination{Sid: paramStr(m["sid"]), StoredSid: paramStr(m["storedSid"]), Profile: paramStr(m["profile"]), ProfileHome: paramStr(m["profileHome"]), OwnerHome: paramStr(m["ownerHome"])}
}

// Flushes a directory entry change to disk outside Windows
func syncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Journals a queued input atomically so it survives a crash, skipped without an id or session
func (p *pendingInputs) save(item queueItem) error {
	if item.SubmissionID == "" || item.Destination == nil || item.Destination.Sid == "" {
		return nil
	}
	dir := pendingDirectory(item.Destination.ProfileHome)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, item.SubmissionID+".json")
	temporary := path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	ownerOf := item.OwnerDestination
	if ownerOf == nil {
		ownerOf = item.Destination
	}
	owner := p.owner(*ownerOf)
	destination := &owner
	if item.InFlight || item.Failed {
		destination = item.Destination
	}
	record := map[string]any{"submissionId": item.SubmissionID, "destination": destinationJSON(destination), "ownerDestination": destinationJSON(&owner), "display": item.Display, "text": item.Text, "attempted": item.InFlight || item.Failed}
	if item.PreparedText != nil {
		record["preparedText"] = *item.PreparedText
	}
	if item.Attachments != nil {
		list := make([]map[string]any, len(item.Attachments))
		for i, a := range item.Attachments {
			list[i] = map[string]any{"path": a.Path, "mime": a.Mime}
		}
		record["attachments"] = list
	}
	if item.ControlMethod != "" {
		record["controlMethod"] = item.ControlMethod
	}
	if item.ExecutionGeneration != nil {
		record["executionGeneration"] = *item.ExecutionGeneration
	}
	if item.LegacyAttempted != nil {
		record["legacyAttempted"] = *item.LegacyAttempted
	}
	if item.CreatedAt != nil {
		record["createdAt"] = *item.CreatedAt
	}
	if item.Queued != nil {
		record["queued"] = *item.Queued
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(record); err != nil {
		return err
	}
	f, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

// Drops a delivered input from the journal
func (p *pendingInputs) remove(item queueItem) error {
	if item.SubmissionID == "" {
		return nil
	}
	profileHome := ""
	if item.Destination != nil {
		profileHome = item.Destination.ProfileHome
	}
	dir := pendingDirectory(profileHome)
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	if err := os.Remove(filepath.Join(dir, item.SubmissionID+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDirectory(dir)
}

// Journaled inputs owned by a destination, oldest first, matched by stored id on a durable resume
func (p *pendingInputs) load(destination submissionDestination, durableResume bool) ([]queueItem, error) {
	dir := pendingDirectory(destination.ProfileHome)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if _, statErr := os.Stat(dir); statErr != nil {
			return nil, nil
		}
		return nil, err
	}
	sessionOf := func(d *submissionDestination) string {
		if d.StoredSid != "" {
			return d.StoredSid
		}
		return d.Sid
	}
	items := []queueItem{}
	for _, entry := range entries {
		name := entry.Name()
		if !pendingRecordName.MatchString(name) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var record map[string]any
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, err
		}
		text, textOK := record["text"].(string)
		display, displayOK := record["display"].(string)
		if jsTemplate(record, "submissionId")+".json" != name || !textOK || !displayOK {
			return nil, errors.New(tr("canonical.queue.invalidRecord", name))
		}
		owner := destinationOf(record["ownerDestination"])
		if record["ownerDestination"] == nil {
			owner = destinationOf(record["destination"])
		}
		if owner == nil {
			owner = &submissionDestination{}
		}
		sameSession := owner.Sid == destination.Sid
		if durableResume {
			sameSession = sessionOf(owner) == sessionOf(&destination)
		}
		if !sameSession || owner.ProfileHome != destination.ProfileHome || owner.Profile != destination.Profile {
			continue
		}
		item := queueItem{Display: display, Text: text, SubmissionID: paramStr(record["submissionId"]), Destination: destinationOf(record["destination"]), OwnerDestination: destinationOf(record["ownerDestination"]), Failed: truthyJS(record["attempted"]), ControlMethod: paramStr(record["controlMethod"])}
		if v, isStr := record["preparedText"].(string); isStr {
			item.PreparedText = &v
		}
		if list, isList := record["attachments"].([]any); isList {
			item.Attachments = []imageAttachment{}
			for _, raw := range list {
				a, _ := raw.(map[string]any)
				item.Attachments = append(item.Attachments, imageAttachment{Path: paramStr(a["path"]), Mime: paramStr(a["mime"])})
			}
		}
		item.ExecutionGeneration, item.CreatedAt = optionalNumber(record, "executionGeneration"), optionalNumber(record, "createdAt")
		if v, isBool := record["legacyAttempted"].(bool); isBool {
			item.LegacyAttempted = &v
		}
		if v, isBool := record["queued"].(bool); isBool {
			item.Queued = &v
		}
		items = append(items, item)
	}
	createdAt := func(i queueItem) float64 {
		if i.CreatedAt == nil {
			return 0
		}
		return *i.CreatedAt
	}
	sort.SliceStable(items, func(a, b int) bool { return createdAt(items[a]) < createdAt(items[b]) })
	return items, nil
}
