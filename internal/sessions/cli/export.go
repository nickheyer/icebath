package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/sessions/export"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
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
	command := &cobra.Command{Use: "export [OUTPUT]", Short: "Save sessions to a file",
		Long: "Writes sessions to OUTPUT, a file or a folder, or to standard output for -. jsonl and html need an OUTPUT, and md and qmd go " +
			"to the session-exports folder when you give none. With --session-id, export writes one session, with filters the sessions " +
			"that match, and with neither every session. Filtered exports include pinned and archived sessions, so they back up " +
			"everything a matching prune could delete. " + times + "\n\n" +
			"trace writes the Claude Code format for the Hugging Face Agent Trace Viewer, and --upload sends it to your Hugging Face " +
			"dataset with HF_TOKEN. trace always hides secrets unless you pass --no-redact. The other formats hide them only with --redact.",
		Example: "  icebath sessions export backup.jsonl\n  icebath sessions export --session-id 3f2a9c --format md\n" +
			"  icebath sessions export old.jsonl --older-than 90",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r := at(cmd)
			output := ""
			if len(args) == 1 {
				output = args[0]
			}
			switch format {
			case "jsonl", "md", "qmd", "html", "trace":
			default:
				return fmt.Errorf("--format takes jsonl, md, qmd, html or trace, not “%s”.", format)
			}
			if only != "" && only != "user-prompts" {
				return fmt.Errorf("--only takes user-prompts, not “%s”.", only)
			}
			if lineage != "single" && lineage != "logical" {
				return fmt.Errorf("--lineage takes single or logical, not “%s”.", lineage)
			}
			var filter *sessionsv1.Filter
			if f.any() {
				var err error
				if filter, err = f.build(""); err != nil {
					return err
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
	command.Flags().StringVar(&format, "format", "jsonl", "File format: jsonl, md, qmd, html or trace")
	command.Flags().BoolVar(&upload, "upload", false, "With trace, upload to your Hugging Face traces dataset instead of a file")
	command.Flags().BoolVar(&public, "public", false, "With --upload, make the dataset public instead of private")
	command.Flags().BoolVar(&noRedact, "no-redact", false, "With trace, keep secrets in, only after you checked the sessions")
	command.Flags().StringVar(&only, "only", "", "Export only part of each session: user-prompts for your messages")
	command.Flags().StringVar(&sessionID, "session-id", "", "Session to export, by id or the unique start of one")
	f.bind(command, "Only sessions older than this time, such as 30 for 30 days, 5h or a date")
	command.Flags().BoolVar(&redactFlag, "redact", false, "Hide API keys, tokens and passwords in the file")
	command.Flags().StringVar(&lineage, "lineage", "single", "With md or qmd: single, or logical to include the sessions it continues")
	command.Flags().BoolVar(&deleteAfter, "delete-after-verified", false, "With md or qmd, delete the one exported session once checked, needs --yes")
	command.Flags().BoolVar(&force, "force", false, "With md or qmd, replace an export file that is there")
	parent.AddCommand(command)
}

// Sessions of a filter, ended ones only, as an export previews them
func (r *run) matching(filter *sessionsv1.Filter) ([]*sessionsv1.Session, error) {
	response, err := r.service.Prune(r.ctx(), &sessionsv1.PruneRequest{Filter: filter, DryRun: true})
	if err != nil {
		return nil, errors.New(strs.Sentence(tui.Reason(err)))
	}
	return response.GetSessions(), nil
}

// Preview of what a filtered export would write
func (r *run) previewExport(filter *sessionsv1.Filter, list []*sessionsv1.Session) {
	r.printf("Would export %s, which match %s. Nothing was written, because you passed --dry-run.", strs.Count(len(list), "session"), describe(filter))
	var table [][]string
	for _, s := range list[:min(100, len(list))] {
		table = append(table, []string{s.GetId(), cmp.Or(s.GetSource(), "Unknown")})
	}
	if len(table) > 0 {
		cli.NewConsole(r.cmd).Table([]string{"ID", "Source"}, table)
	}
	if len(list) > 100 {
		r.printf("  And %s more.", strs.Number(float64(len(list)-100)))
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
		return nil, false, errors.New("--dry-run needs a filter, such as --older-than 90.")
	default:
		request.Filter = &sessionsv1.Filter{IncludeArchived: true, IncludePinned: true, IncludeHidden: true}
	}
	response, err := r.service.Export(r.ctx(), request)
	if err != nil {
		return nil, false, errors.New(strs.Sentence(tui.Reason(err)))
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
			return errors.New("--only user-prompts works with --format jsonl or md.")
		}
	}
	switch {
	case view == "html" && (output == "" || output == "-"):
		return errors.New("An HTML export needs the path of the file to write, such as icebath sessions export sessions.html --format html.")
	case view == "jsonl" && !onlyPrompts && output == "":
		return errors.New("A JSONL export needs the path of the file to write, or - to print it, such as icebath sessions export backup.jsonl.")
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
		r.printf("✓ Exported %s to %s.", strs.Count(prompts, "message"), path)
	case onlyPrompts:
		r.printf("✓ Exported your messages of %s to %s.", strs.Count(len(transcripts), "session"), path)
	case view == "html":
		r.printf("✓ Exported %s as a web page to %s.", strs.Count(len(transcripts), "session"), path)
	default:
		r.printf("✓ Exported %s to %s.", strs.Count(len(transcripts), "session"), path)
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
				r.print("You have no session to upload yet.")
				return nil
			}
			return errors.New("You have no session to export yet.")
		}
		id = response.GetSessions()[0].GetId()
	}
	if upload {
		if id == "" {
			return errors.New("--upload sends one session. Pass --session-id, or leave out the filters to send the newest one.")
		}
		transcripts, _, err := r.collect(id, nil, f, false, false)
		if err != nil {
			r.printf("✗ The session %s could not be loaded: %s", id, strs.Sentence(err.Error()))
			return nil
		}
		if len(transcripts) == 0 || len(transcripts[0].GetMessages()) == 0 {
			r.print("This session has no messages to upload yet.")
			return nil
		}
		trace, err := export.Trace(transcripts[0], mask)
		if err != nil {
			r.print("✗ Nothing was uploaded, because hiding the secrets failed, so the session may still hold API keys or other private data. " +
				"Check the session yourself, and only then run the command again with --no-redact.")
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
			return fmt.Errorf("The session %s has no messages to export yet.", id)
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
			r.printf("✓ Exported the trace of the session to %s.", path)
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
	r.printf("✓ Exported the traces of %s to %s.", strs.Count(written, "session"), dir)
	return nil
}

// Markdown or QMD transcripts that carry their own digest, one file each with a manifest, and with delete the session removed once its
// file verifies
func (r *run) exportMemory(output, format, id string, filter *sessionsv1.Filter, f *filters, mask, lineage, deleteAfter, force bool) error {
	if output == "-" {
		return errors.New("Markdown and QMD exports write files, so - to print works only with --format jsonl.")
	}
	dir, err := folder(output)
	if err != nil {
		return err
	}
	if deleteAfter && !f.yes {
		return errors.New("--delete-after-verified needs --yes too.")
	}
	if deleteAfter && id == "" {
		return errors.New("--delete-after-verified works only with --session-id.")
	}
	if id == "" && filter == nil {
		return errors.New("A Markdown or QMD export of many sessions needs a filter. Pass --session-id, or a filter such as --older-than 90 or " +
			"--source telegram.")
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
			return errors.New(strs.Sentence(tui.Reason(err)))
		}
		segments := response.GetTranscripts()
		if len(segments) == 0 {
			if id != "" {
				return fmt.Errorf("The session %s was gone before it could be exported, so nothing was deleted.", head)
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
				return fmt.Errorf("%s exists already. Pass --force to replace it.", path)
			}
			r.printf("Skipped %s, which exists already. Pass --force to replace it.", path)
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
		r.printf("✓ Exported %s to %s.", strs.Count(len(files), "session"), dir)
		return nil
	}
	destination := dir
	if len(files) == 1 {
		destination = files[0].path
	}
	r.printf("✓ Exported %s with %s to %s.", strs.Count(len(files), "session"), strs.Count(messages, "message"), destination)
	if !deleteAfter {
		return nil
	}
	for _, file := range files {
		data, err := os.ReadFile(file.path)
		if err != nil {
			return fmt.Errorf("The session %s was not deleted, because its export file %s is missing.", id, file.path)
		}
		if outcome := export.Verify(data, file.id, file.count); outcome != "ok" {
			return fmt.Errorf("The session %s was not deleted, because its export did not check out: %s", id, strs.Sentence(outcome))
		}
	}
	deleted, err := r.service.Delete(r.ctx(), &sessionsv1.DeleteRequest{Id: id})
	if err != nil {
		if strings.Contains(tui.Reason(err), "in the middle of a reply") {
			return fmt.Errorf("The session %s was exported but not deleted, because it, or one of its subagents, is in the middle of a reply or of "+
				"being summarized.", id)
		}
		return fmt.Errorf("The session %s was exported but not deleted: %s", id, strs.Sentence(tui.Reason(err)))
	}
	others := len(deleted.GetIds()) - 1
	line := fmt.Sprintf("✓ Deleted the exported session %s", id)
	if others > 0 {
		line += " and " + strs.Count(others, "related session")
	}
	r.print(line + ".")
	return nil
}
