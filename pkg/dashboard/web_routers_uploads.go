package dashboard

import (
	"context"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/stageduploads"
)

var uploadsMaxUploadBytes int64 = home.WebappAttachmentMaxBytes

// Router staging one browser picked file under the requested profile home
func uploadsRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Post("/api/chat/file-upload", &Endpoint{
		Name: "upload_chat_file",
		Doc: `Stage one user-selected browser file under the active icebath profile.

    The client never chooses a server path. A unique 0600 file under
    ` + "``$ICEBATH_HOME/uploads``" + ` is returned for the existing ` + "``file.attach``" + ` flow.
    `,
		Params: []Param{QueryParam("profile", Optional(Str), nil)},
		Body:   []BodyParam{FileField("file")},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			file, _ := c.Value("file").(*UploadFile)
			return uploadChatFile(ctx, file, c.Str("profile"))
		},
	})
	return r
}

// Stages one picked file under the profile home, 413 past the cap, with its receipt when an install id exists
func uploadChatFile(ctx context.Context, file *UploadFile, profile string) (*pyjson.Object, error) {
	profileHome, err := resolveUploadHome(ctx, profile)
	if err != nil {
		return nil, err
	}
	var total int64
	var target string
	err = func() error {
		defer file.Close()
		var err error
		if total, err = file.File.Seek(0, io.SeekEnd); err != nil {
			return err
		}
		if total > uploadsMaxUploadBytes {
			return NewHTTPException(413, fmt.Sprintf("File is too large; cap is %d MiB", uploadsMaxUploadBytes/(1024*1024)))
		}
		name := "attachment"
		if file.Filename != nil && *file.Filename != "" {
			name = *file.Filename
		}
		target, err = publishStagedUpload(ctx, file.File, profileHome, name)
		return err
	}()
	if err != nil {
		return nil, err
	}
	result := pyjson.ObjectOf("ok", true, "path", target, "size", total)
	descriptor, err := stageduploads.StagedUploadDescriptor(ctx, target, profileHome)
	if err != nil {
		return nil, err
	}
	if descriptor != nil {
		result.Set("staged_upload", descriptor)
	}
	return result, nil
}
