package dashboard

import (
	"context"
	"errors"
	"io/fs"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Path bound download or media ticket for a browser URL instead of the session token
func issueFileTicket(ctx context.Context, payload *pyjson.Object, req *Request) (any, error) {
	route := objectStr(payload, "route")
	rawPath := objectStr(payload, "path")
	profilePtr := objectOptStr(payload, "profile")
	profile := ""
	if profilePtr != nil {
		profile = *profilePtr
	}
	sessionID := objectOptStr(payload, "session_id")
	authority, err := func() (*FileAuthority, error) {
		owner, err := historyProfileHome(ctx, profile)
		if err != nil {
			return nil, err
		}
		session, err := fsOwnedSession(ctx, sessionID, profile)
		if err != nil {
			return nil, err
		}
		if route == "download" {
			return fsDownloadAuthority(ctx, req, rawPath, profile, sessionID, owner, session)
		}
		authority, err := remoteFileAuthority(ctx, rawPath, profile, owner, remoteStreamTarget)
		if err != nil || authority != nil {
			return authority, err
		}
		path := rawPath
		if sessionID != nil || strings.HasPrefix(strings.ToLower(path), "file:") {
			if path, err = fsDownloadPath(ctx, path, profile, sessionID, session, true, false); err != nil {
				return nil, err
			}
		}
		_, target, _, _, _, err := managedReadableFile(ctx, path, false)
		if err != nil {
			return nil, err
		}
		return liveFileAuthority(target, owner)
	}()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, NewHTTPException(404, "File ticket profile is unavailable")
	}
	if err != nil {
		return nil, err
	}
	query := [][2]string{{"path", rawPath}}
	if profilePtr != nil {
		query = append(query, [2]string{"profile", *profilePtr})
	}
	if sessionID != nil {
		query = append(query, [2]string{"session_id", *sessionID})
	}
	var token *string
	if t, ok := sessionTokenOf(req); ok {
		token = &t
	}
	ticket := mintFileTicket(route, query, *authority, token)
	return JSONResponse(pyjson.ObjectOf("ticket", ticket), 200, "Cache-Control", "no-store")
}

// Managed or SSH workspace media streamed inline with Range support, a ticket pinning the file
func streamManagedFile(ctx context.Context, req *Request, path, profile string, sessionID *string) (any, error) {
	session, err := fsOwnedSession(ctx, sessionID, profile)
	if err != nil {
		return nil, err
	}
	authority, ticketed := req.State().Get("file_ticket_authority").(FileAuthority)
	if ticketed {
		if workspace, ok := authority.Workspace.(*SshWorkspaceFs); ok && workspace != nil {
			target, size, err := ticketedRemoteTarget(authority, remoteStreamTarget)
			if err != nil {
				return nil, err
			}
			return remoteMediaResponse(req, workspace, target, size, fsMimeType(target))
		}
	} else {
		backend, err := remoteFsBackend(ctx, path, profile)
		if err != nil {
			return nil, err
		}
		if backend != nil {
			target, size, err := remoteStreamTarget(backend, path)
			if err != nil {
				return nil, err
			}
			return remoteMediaResponse(req, backend, target, size, fsMimeType(target))
		}
		if sessionID != nil || strings.HasPrefix(strings.ToLower(path), "file:") {
			if path, err = fsDownloadPath(ctx, path, profile, sessionID, session, true, false); err != nil {
				return nil, err
			}
		}
	}
	return managedFileResponse(ctx, req, path, "inline", true)
}
