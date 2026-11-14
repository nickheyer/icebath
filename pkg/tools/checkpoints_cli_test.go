package tools

import (
	"bytes"
	"errors"
	"testing"
)

func checkpointsNs(over map[string]any) map[string]any {
	ns := map[string]any{"retention_days": 7, "max_size_mb": 500, "keep_orphans": false, "force": false}
	for k, v := range over {
		ns[k] = v
	}
	return ns
}

func checkpointsPreV2OnlyStatus() CheckpointStatus {
	return CheckpointStatus{PreV2Projects: []CheckpointPreV2Status{{Path: "/home/user/.icebath/checkpoints/deadbeefcafebabe", Exists: false}}}
}

func checkpointsMixedStatus() CheckpointStatus {
	workdir := "/gone/pre-v2-project"
	return CheckpointStatus{
		Projects:      []CheckpointProjectStatus{{Hash: "abc123", Workdir: "/gone/v2-project", Exists: false, Commits: 4}},
		PreV2Projects: []CheckpointPreV2Status{{Path: "/home/user/.icebath/checkpoints/deadbeefcafebabe", Workdir: &workdir, Exists: false}},
	}
}

// Swaps the store status and prune seams, recording every prune call the way the hermes fixture does
func patchCheckpointManager(t *testing.T, status CheckpointStatus) *[]CheckpointPruneOptions {
	t.Helper()
	var calls []CheckpointPruneOptions
	savedStatus, savedPrune, savedInput := checkpointsStoreStatusFn, checkpointsPruneFn, checkpointsInputFn
	checkpointsStoreStatusFn = func(*Env, string) (CheckpointStatus, error) { return status, nil }
	checkpointsPruneFn = func(_ *Env, o CheckpointPruneOptions) (CheckpointPruneResult, error) {
		calls = append(calls, o)
		return CheckpointPruneResult{DeletedOrphan: len(status.Projects) + len(status.PreV2Projects)}, nil
	}
	t.Cleanup(func() {
		checkpointsStoreStatusFn, checkpointsPruneFn, checkpointsInputFn = savedStatus, savedPrune, savedInput
	})
	return &calls
}

func TestKeepOrphansSkipsPrompt(t *testing.T) {
	cases := map[string]CheckpointStatus{"pre_v2_only": checkpointsPreV2OnlyStatus(), "mixed": checkpointsMixedStatus()}
	for name, status := range cases {
		t.Run(name, func(t *testing.T) {
			calls := patchCheckpointManager(t, status)
			checkpointsInputFn = func(string) (string, error) {
				t.Fatal("input() must not be called when --keep-orphans is passed")
				return "", errors.New("unreachable")
			}
			var out bytes.Buffer
			rc, err := checkpointsCmdPrune(&Env{}, checkpointsNs(map[string]any{"keep_orphans": true}), &out)
			if err != nil || rc != 0 {
				t.Fatalf("rc %d err %v", rc, err)
			}
			if len(*calls) != 1 || (*calls)[0].DeleteOrphans {
				t.Fatalf("prune calls %+v", *calls)
			}
		})
	}
}

func TestEmptyPreviewBindsEmptyAllowlist(t *testing.T) {
	calls := patchCheckpointManager(t, CheckpointStatus{})
	var out bytes.Buffer
	rc, err := checkpointsCmdPrune(&Env{}, checkpointsNs(nil), &out)
	if err != nil || rc != 0 {
		t.Fatalf("rc %d err %v", rc, err)
	}
	if len(*calls) != 1 || (*calls)[0].OrphanAllowlist == nil || len((*calls)[0].OrphanAllowlist) != 0 {
		t.Fatalf("prune calls %+v", *calls)
	}
}
