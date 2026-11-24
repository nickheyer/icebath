package memory

import (
	"context"
	"testing"
	"time"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

var (
	CurTestHome                  = curTestHome
	CurBackupHome                = curBackupHome
	CurWriteSkill                = curWriteSkill
	CurBackdate                  = curBackdate
	CurRecord                    = curRecord
	CurGet                       = curGet
	CurToolCall                  = curToolCall
	CurRows                      = curRows
	CurRunJSON                   = curRunJSON
	CuratorRestoreCronSkillLinks = curatorRestoreCronSkillLinks
	CuratorCronJobsFilename      = curatorCronJobsFilename
	CuratorCount                 = curatorCount
)

// Writes a run report for the external cron tests through the unexported report writer
func CurWriteReport(t *testing.T, ctx context.Context, counts string, before, after []*jsonutil.Object, names map[string]bool, meta *jsonutil.Object) string {
	t.Helper()
	return curatorWriteRunReport(ctx, curReportInput(time.Now().UTC(), counts, before, after, names, meta))
}

// LLM meta with the final text and tool calls the classification reads
func CurMetaWith(final string, toolCalls []any) *jsonutil.Object {
	return curatorEntry("final", final, "summary", "short summary", "model", "test-model", "provider", "test-provider", "tool_calls", toolCalls, "error", nil)
}
