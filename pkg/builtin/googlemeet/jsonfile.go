package googlemeet

import (
	"fmt"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Parsed JSON from path, nil when missing, unreadable or malformed
func readJSON(path string) any {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text, err := textutil.DecodeUTF8(textutil.TrimBOM(raw))
	if err != nil {
		return nil
	}
	v, err := jsonutil.Decode(text)
	if err != nil {
		return nil
	}
	return v
}

// Cross process lock around say_queue.jsonl edits so an append never lands between a re read and a rewrite
var sayQueueLock = func(queuePath string) (func(), error) {
	lockPath := queuePath + ".lock"
	return provider.FileLock(lockPath, 10*time.Second, fmt.Sprintf("Timed out waiting for say queue lock (%s)", lockPath))
}
