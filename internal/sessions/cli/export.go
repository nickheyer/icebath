package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/sessions/export"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/tui"
)

// Folder exports are written to when no output is given
func exportsDir() (string, error) {
	return export.Folder("session-exports")
}

// Path of an output: the file named, or the default name inside a folder named, the folder made as needed
func inFolder(output, name string) (string, error) {
	if output == "" || output == "-" {
		return output, nil
	}
	info, err := os.Stat(output)
	if strings.HasSuffix(output, string(filepath.Separator)) || err == nil && info.IsDir() {
		if err := os.MkdirAll(output, 0o755); err != nil {
			return "", err
		}
		return filepath.Join(output, name), nil
	}
	return output, nil
}

// Folder an output names, the default exports folder for none
func folder(output string) (string, error) {
	if output == "" || output == "-" {
		return exportsDir()
	}
	if rest, ok := strings.CutPrefix(output, "~"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		output = home + rest
	}
	return filepath.Clean(output), nil
}

func addExport(parent *cobra.Command, at func(*cobra.Command) *run) {
	var f filters
	var format, only, sessionID, lineage string
	var upload, public, noRedact, redactFlag, deleteAfter, force bool
	command := &cobra.Command{Use: "export [OUTPUT]", Short: "Export sessions to JSONL, Markdown, or QMD", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r := at(cmd)
			output := ""
			if len(args) == 1 {
				output = args[0]
			}
			switch format {
			case "jsonl", "md", "qmd", "html", "trace":
			default:
				return fmt.Errorf("invalid choice: '%s' (choose from 'jsonl', 'md', 'qmd', 'html', 'trace')", format)
			}
			if only != "" && only != "user-prompts" {
				return fmt.Errorf("invalid choice: '%s' (choose from 'user-prompts')", only)
			}
			if lineage != "single" && lineage != "logical" {
				return fmt.Errorf("invalid choice: '%s' (choose from 'single', 'logical')", lineage)
			}
			var filter *sessionsv1.Filter
			if f.any() {
				var err error
				if filter, err = f.build(""); err != nil {
					return errors.New("Error: " + err.Error())
				}
				filter.IncludeArchived, filter.IncludePinned = true, true
			}
			id := ""
			if sessionID != "" {
				session, err := r.resolve(sessionID)
				if err != nil {
					return err
				}
				id = session.GetId()
			}
			switch {
			case only != "":
				return r.exportFlat(output, "only:"+format, id, filter, &f, redactFlag)
			case format == "trace":
				return r.exportTrace(output, id, filter, &f, upload, public, !noRedact)
			case format == "jsonl" || format == "html":
				return r.exportFlat(output, format, id, filter, &f, redactFlag)
			}
			return r.exportMemory(output, format, id, filter, &f, redactFlag, lineage == "logical", deleteAfter, force)
		}}
	command.Flags().StringVar(&format, "format", "jsonl", "Export format (default: jsonl). 'trace' emits Claude Code JSONL for the Hugging Face Agent Trace Viewer")
	command.Flags().BoolVar(&upload, "upload", false, "trace only: upload to your Hugging Face traces dataset instead of writing a local file (needs HF_TOKEN)")
	command.Flags().BoolVar(&public, "public", false, "trace --upload only: create/update a public dataset instead of private")
	command.Flags().BoolVar(&noRedact, "no-redact", false, "trace only: skip the forced secret redaction; only use after manual review")
	command.Flags().StringVar(&only, "only", "", "Export only a filtered view (user-prompts: one prompt record per line for jsonl, headed sections for md)")
	command.Flags().StringVar(&sessionID, "session-id", "", "Session ID or unique prefix to export")
	f.bind(command, "Only export sessions older than AGE (duration like '5h'/'2d', bare number of days, or an ISO timestamp). Filtered exports include pinned "+
		"and archived sessions, so they back up everything a matching prune could delete")
	command.Flags().BoolVar(&redactFlag, "redact", false, "Redact secrets (API keys, tokens, credentials) from exported content")
	command.Flags().StringVar(&lineage, "lineage", "single", "md/qmd only: export one row or its compression lineage")
	command.Flags().BoolVar(&deleteAfter, "delete-after-verified", false, "md/qmd only: after verified single-session export, delete that session (needs --yes)")
	command.Flags().BoolVar(&force, "force", false, "md/qmd only: overwrite an existing export file")
	parent.AddCommand(command)
}

// Sessions of a filter, ended ones only, as an export previews them
func (r *run) matching(filter *sessionsv1.Filter) ([]*sessionsv1.Session, error) {
	response, err := r.service.Prune(r.ctx(), &sessionsv1.PruneRequest{Filter: filter, DryRun: true})
	if err != nil {
		return nil, errors.New("Error: " + tui.Reason(err))
	}
	return response.GetSessions(), nil
}

// Preview of what a filtered export would write
func (r *run) previewExport(filter *sessionsv1.Filter, list []*sessionsv1.Session) {
	r.printf("Would export %d session(s) (%s).", len(list), describe(filter))
	for _, s := range list[:min(100, len(list))] {
		r.printf("  %s  %s", s.GetId(), s.GetSource())
	}
	if len(list) > 100 {
		r.printf("  … %d more", len(list)-100)
	}
}

// Transcripts to write: the session named, the sessions a filter matches, or every session
func (r *run) collect(id string, filter *sessionsv1.Filter, f *filters, capped, lineage bool) ([]*sessionsv1.Transcript, bool, error) {
	request := &sessionsv1.ExportRequest{Capped: capped, Lineage: lineage}
	switch {
	case id != "":
		request.Filter = &sessionsv1.Filter{Ids: []string{id}}
	case filter != nil:
		list, err := r.matching(filter)
		if err != nil {
			return nil, false, err
		}
		if f.dryRun {
			r.previewExport(filter, list)
			return nil, true, nil
		}
		ids := make([]string, len(list))
		for i, s := range list {
			ids[i] = s.GetId()
		}
		if len(ids) == 0 {
			return nil, false, nil
		}
		request.Filter = &sessionsv1.Filter{Ids: ids}
	case f.dryRun:
		return nil, false, errors.New("--dry-run requires at least one filter.")
	default:
		request.Filter = &sessionsv1.Filter{IncludeArchived: true, IncludePinned: true, IncludeHidden: true}
	}
	response, err := r.service.Export(r.ctx(), request)
	if err != nil {
		return nil, false, errors.New("Error: " + tui.Reason(err))
	}
	return response.GetTranscripts(), false, nil
}

// Writes the text where the output says, stdout for none, and whether it went to a file
func (r *run) write(path string, data []byte) (bool, error) {
	if path == "" || path == "-" {
		_, err := r.cmd.OutOrStdout().Write(data)
		return false, err
	}
	return true, os.WriteFile(path, data, 0o644)
}

// JSONL backup, HTML page or the user's prompts, written to one file
func (r *run) exportFlat(output, format, id string, filter *sessionsv1.Filter, f *filters, mask bool) error {
	view, onlyPrompts := format, strings.HasPrefix(format, "only:")
	if onlyPrompts {
		view = strings.TrimPrefix(format, "only:")
		if view != "jsonl" && view != "md" {
			return errors.New("--only user-prompts supports --format jsonl or md.")
		}
	}
	switch {
	case view == "html" && (output == "" || output == "-"):
		return errors.New("HTML export requires an output file path.")
	case view == "jsonl" && !onlyPrompts && output == "":
		return errors.New("JSONL export requires an output path (use - for stdout).")
	}
	transcripts, previewed, err := r.collect(id, filter, f, view == "jsonl" && !onlyPrompts, false)
	if err != nil || previewed {
		return err
	}
	if id != "" && len(transcripts) == 0 {
		return notFound(id)
	}
	if mask {
		transcripts = export.Redact(transcripts)
	}
	if onlyPrompts || view == "html" {
		for _, t := range transcripts {
			t.Messages = export.Kept(t.GetMessages())
		}
	}
	name := "icebath_sessions." + view
	if len(transcripts) == 1 {
		name = export.DefaultName(transcripts[0].GetSession().GetId(), view)
	}
	path, err := inFolder(output, name)
	if err != nil {
		return err
	}
	var data []byte
	prompts := 0
	switch {
	case onlyPrompts && view == "jsonl":
		data, prompts, err = export.PromptsJSONL(transcripts)
	case onlyPrompts:
		data = export.PromptsMarkdown(transcripts)
	case view == "html":
		data, err = export.HTML(transcripts)
	default:
		data, err = export.JSONL(transcripts)
	}
	if err != nil {
		return err
	}
	toFile, err := r.write(path, data)
	if err != nil || !toFile {
		return err
	}
	switch {
	case onlyPrompts && view == "jsonl":
		r.printf("Exported %s to %s", plural(prompts, "prompt"), path)
	case onlyPrompts:
		r.printf("Exported %s to %s", plural(len(transcripts), "session"), path)
	case view == "html":
		r.printf("Exported %s to %s (HTML)", plural(len(transcripts), "session"), path)
	case id != "":
		r.printf("Exported %d session to %s", len(transcripts), path)
	default:
		r.printf("Exported %d sessions to %s", len(transcripts), path)
	}
	return nil
}

// Claude Code JSONL traces, written locally or uploaded to Hugging Face
func (r *run) exportTrace(output, id string, filter *sessionsv1.Filter, f *filters, upload, public, mask bool) error {
	if id == "" && filter == nil {
		response, err := r.service.List(r.ctx(), &sessionsv1.ListRequest{Filter: &sessionsv1.Filter{Limit: 1}})
		if err != nil {
			return err
		}
		if len(response.GetSessions()) == 0 {
			if upload {
				r.print("No active session to upload.")
				return nil
			}
			return errors.New("No session found to export. Pass --session-id.")
		}
		id = response.GetSessions()[0].GetId()
	}
	if upload {
		if id == "" {
			return errors.New("--upload exports one session: pass --session-id (or drop filters to use the most recent).")
		}
		transcripts, _, err := r.collect(id, nil, f, false, false)
		if err != nil {
			r.printf("Could not load session %s: %s", id, err)
			return nil
		}
		if len(transcripts) == 0 || len(transcripts[0].GetMessages()) == 0 {
			r.print("No transcript to upload for this session yet.")
			return nil
		}
		trace, err := export.Trace(transcripts[0], mask)
		if err != nil {
			r.print("Trace upload blocked: secret redaction failed, so the transcript may still contain credentials or other sensitive data. Fix the redactor " +
				"or rerun with --no-redact only after manually reviewing the transcript.")
			return nil
		}
		ctx, cancel := context.WithTimeout(r.ctx(), 10*time.Minute)
		defer cancel()
		r.print(export.Upload(ctx, id, trace, public))
		return nil
	}
	transcripts, previewed, err := r.collect(id, filter, f, false, false)
	if err != nil || previewed {
		return err
	}
	if id != "" {
		if len(transcripts) == 0 || len(transcripts[0].GetMessages()) == 0 {
			return fmt.Errorf("No transcript to export for session '%s'.", id)
		}
		trace, err := export.Trace(transcripts[0], mask)
		if err != nil {
			return err
		}
		path, err := inFolder(output, id+".trace.jsonl")
		if err != nil {
			return err
		}
		toFile, err := r.write(path, trace)
		if err == nil && toFile {
			r.printf("Exported 1 session trace to %s", path)
		}
		return err
	}
	dir, err := folder(output)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	written := 0
	for _, t := range transcripts {
		trace, err := export.Trace(t, mask)
		if err != nil {
			return err
		}
		if len(trace) == 0 {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, t.GetSession().GetId()+".trace.jsonl"), trace, 0o644); err != nil {
			return err
		}
		written++
	}
	r.printf("Exported %d session trace(s) to %s", written, dir)
	return nil
}

// Markdown or QMD transcripts that carry their own digest, one file each with a manifest, and with delete the session removed once its
// file verifies
func (r *run) exportMemory(output, format, id string, filter *sessionsv1.Filter, f *filters, mask, lineage, deleteAfter, force bool) error {
	if output == "-" {
		return errors.New("Markdown/QMD export writes files; stdout (-) is only supported with --format jsonl.")
	}
	dir, err := folder(output)
	if err != nil {
		return err
	}
	if deleteAfter && !f.yes {
		return errors.New("--delete-after-verified requires --yes.")
	}
	if deleteAfter && id == "" {
		return errors.New("--delete-after-verified is only supported with --session-id.")
	}
	if id == "" && filter == nil {
		return errors.New("Refusing bulk export without a filter. Pass --session-id or at least one filter (e.g. --older-than 90, --source telegram).")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var heads []string
	if id != "" {
		heads = []string{id}
	} else {
		list, err := r.matching(filter)
		if err != nil {
			return err
		}
		if f.dryRun {
			r.previewExport(filter, list)
			return nil
		}
		for _, s := range list {
			heads = append(heads, s.GetId())
		}
	}
	type written struct {
		path, id string
		count    int
	}
	var files []written
	messages := 0
	for _, head := range heads {
		response, err := r.service.Export(r.ctx(), &sessionsv1.ExportRequest{Filter: &sessionsv1.Filter{Ids: []string{head}}, Lineage: lineage})
		if err != nil {
			return errors.New("Error: " + tui.Reason(err))
		}
		segments := response.GetTranscripts()
		if len(segments) == 0 {
			if id != "" {
				return fmt.Errorf("Session '%s' disappeared during export; nothing was deleted.", head)
			}
			continue
		}
		for _, t := range segments {
			t.Messages = export.Kept(t.GetMessages())
		}
		if mask {
			segments = export.Redact(segments)
		}
		last := segments[len(segments)-1].GetSession()
		path := filepath.Join(dir, export.FileName(last, format))
		if _, err := os.Stat(path); err == nil && !force {
			if id != "" {
				return fmt.Errorf("Export already exists: %s. Pass --force to overwrite.", path)
			}
			r.printf("Skipping existing export: %s. Pass --force to overwrite.", path)
			continue
		}
		now := time.Now()
		data, count := export.Memory(segments, format, now)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
		var chain []string
		if len(segments) > 1 {
			for _, t := range segments {
				chain = append(chain, t.GetSession().GetId())
			}
		}
		line, err := export.ManifestLine(last, chain, format, path, count, data, now)
		if err != nil {
			return err
		}
		manifest, err := os.OpenFile(filepath.Join(dir, "manifest.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		_, err = manifest.Write(line)
		manifest.Close()
		if err != nil {
			return err
		}
		files = append(files, written{path: path, id: last.GetId(), count: count})
		messages += count
	}
	if id == "" {
		r.printf("Exported %d session(s) to %s", len(files), dir)
		return nil
	}
	destination := dir
	if len(files) == 1 {
		destination = files[0].path
	}
	r.printf("Exported %s (%s) to %s", plural(len(files), "session"), plural(messages, "message"), destination)
	if !deleteAfter {
		return nil
	}
	for _, file := range files {
		data, err := os.ReadFile(file.path)
		if err != nil {
			return fmt.Errorf("Export verification failed; not deleting session '%s': file missing", id)
		}
		if outcome := export.Verify(data, file.id, file.count); outcome != "ok" {
			return fmt.Errorf("Export verification failed; not deleting session '%s': %s", id, outcome)
		}
	}
	deleted, err := r.service.Delete(r.ctx(), &sessionsv1.DeleteRequest{Id: id})
	if err != nil {
		if strings.Contains(tui.Reason(err), "active turn lease or compression lock") {
			return fmt.Errorf("Exported, but not deleted: session '%s' (or a delegate child) has an active turn lease or compression lock", id)
		}
		return fmt.Errorf("Exported, but session '%s' was not deleted: %s", id, tui.Reason(err))
	}
	others := len(deleted.GetIds()) - 1
	line := fmt.Sprintf("Deleted exported session '%s'", id)
	if others > 0 {
		line += fmt.Sprintf(" and %s", plural(others, "related session"))
	}
	r.print(line + ".")
	return nil
}
