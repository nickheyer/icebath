package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Prompt the vision tool describes an inbound image with for a model that cannot read images
const VisionAnalysisPrompt = "Concisely describe this image in 2-4 sentences " +
	"(~200 Chinese characters or ~150 English words). " +
	"Cover the main subject, key visible text/data/code, and overall context. " +
	"If it is a chart, diagram, or scientific figure, include the important " +
	"labels, legend, and key values. Skip decorative details."

// Text with a vision description and the cached path of each attached image prepended
func VisionImageNotes(ctx context.Context, env *tools.Env, userText string, imagePaths []string) string {
	parts := make([]string, 0, len(imagePaths))
	for _, path := range imagePaths {
		pkgLog().Debug("Auto-analyzing user image: " + path)
		var result map[string]any
		if err := json.Unmarshal([]byte(tools.VisionAnalyzeTool(ctx, env, path, VisionAnalysisPrompt)), &result); err != nil {
			pkgLog().Error(fmt.Sprintf("Vision auto-analysis error: %s", err))
			parts = append(parts, "[The user sent an image but something went wrong when I "+
				"tried to look at it~ You can try examining it yourself "+
				"with vision_analyze using image_url: "+path+"]")
			continue
		}
		if pyrepr.Truthy(result["success"]) {
			analysis, _ := result["analysis"].(string)
			parts = append(parts, "[The user sent an image. Here's what I can see:\n"+memory.SanitizeContext(analysis)+"]\n"+
				"[If you need a closer look, use vision_analyze with "+
				"image_url: "+path+"]")
			continue
		}
		parts = append(parts, "[The user sent an image but I couldn't quite see it "+
			"this time. You can try looking at it yourself "+
			"with vision_analyze using image_url: "+path+"]")
	}
	if len(parts) == 0 {
		return userText
	}
	prefix := strings.Join(parts, "\n\n")
	if userText != "" {
		return prefix + "\n\n" + userText
	}
	return prefix
}
