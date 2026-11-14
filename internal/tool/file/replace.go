package file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

func noMatchResult(path, content, oldString, newString string, matchCount int, errText string) PatchResult {
	if alreadyApplied(content, oldString, newString) {
		return PatchResult{Success: true, NoChange: true,
			Note: "File already contains the target text — the edit " +
				"appears to be already applied to " + path + ". No write " +
				"performed; do not re-send this patch."}
	}
	if errText == "" {
		errText = "Could not find match for old_string in " + path
	}
	return PatchResult{Error: errText + NoMatchHint(errText, matchCount, oldString, content)}
}

func (o *Operations) verifyPatchPersisted(ctx context.Context, path, newContent string) (string, error) {
	data, failed, err := o.readExactBytes(ctx, path)
	if err != nil {
		return "", err
	}
	if failed != nil {
		return "Post-write verification failed: could not re-read " + path, nil
	}
	bomless, _ := stripBOM(string(data))
	onDisk := foldLineEndings(bomless)
	intended := foldLineEndings(newContent)
	if onDisk != intended {
		return fmt.Sprintf("Post-write verification failed for %s: on-disk content "+
			"differs from intended write "+
			"(wrote %d chars, read back "+
			"%d chars after normalizing line endings). "+
			"The patch did not persist. Re-read the file and try again.", path, charLen(intended), charLen(onDisk)), nil
	}
	return "", nil
}

func (o *Operations) Replace(ctx context.Context, path, oldString, newString string, replaceAll bool) (PatchResult, error) {
	path, err := o.expandPath(ctx, path)
	if err != nil {
		return PatchResult{}, err
	}
	denied, err := o.guard.WriteDenied(ctx, path, "Write", false)
	if err != nil {
		return PatchResult{}, err
	}
	if denied != "" {
		return PatchResult{Error: denied}, nil
	}
	data, failed, err := o.readExactBytes(ctx, path)
	if err != nil {
		return PatchResult{}, err
	}
	if failed != nil {
		if failed.cwdError != "" {
			return PatchResult{Error: failed.cwdError}, nil
		}
		return PatchResult{Error: "Failed to read file: " + path}, nil
	}
	raw := string(data)
	content, _ := stripBOM(raw)

	found, err := FuzzyReplace(ctx, content, oldString, newString, replaceAll)
	var matchErr *MatchError
	switch {
	case errors.As(err, &matchErr):
		return noMatchResult(path, content, oldString, newString, found.Count, matchErr.Msg), nil
	case err != nil:
		return PatchResult{}, err
	case found.Count == 0:
		return noMatchResult(path, content, oldString, newString, 0, ""), nil
	}
	newContent := found.Content
	if ending := detectLineEnding(content); ending != "" {
		newContent = normalizeLineEndings(newContent, ending)
	}
	written, err := o.Write(ctx, path, newContent, &raw)
	if err != nil {
		return PatchResult{}, err
	}
	if written.Error != "" {
		return PatchResult{Error: "Failed to write changes: " + written.Error}, nil
	}
	mismatch, err := o.verifyPatchPersisted(ctx, path, newContent)
	if err != nil {
		return PatchResult{}, err
	}
	if mismatch != "" {
		return PatchResult{Error: mismatch}, nil
	}
	var notes []string
	if found.Note != "" {
		notes = append(notes, found.Note)
	}
	if note := boundaryNote([]string{found.Strategy}, "old_string's", ""); note != "" {
		notes = append(notes, note)
	}
	readSHA := sha256.Sum256(data)
	return PatchResult{
		Success:        true,
		Diff:           fileDiff(content, newContent, path),
		FilesModified:  []string{path},
		Lint:           written.Lint,
		Note:           strings.Join(notes, "\n"),
		LSPDiagnostics: written.LSPDiagnostics,
		Writes:         []PatchWrite{{Path: path, ReadSHA256: hex.EncodeToString(readSHA[:]), ReadKnown: true, WrittenSHA256: written.ContentSHA256}},
	}, nil
}
