package botmode

import (
	"regexp"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Sender stamp a relayed DM opens with, its handle the second group
var senderStampRe = regexp.MustCompile("(?s)^(Message from \U0001F916 .+? \\(@)([A-Za-z0-9_-]+)(\\): )")

// Relayed DM with its sender stamp naming the form this gateway can reply to, unchanged when it carries no stamp
func QualifySenderStamp(message string, fromHandle, fromConnection any, roster []RemoteRosterRow, localTaken map[string]bool) string {
	handle := pystr.LstripChars(pystr.Strip(strOr(fromHandle)), "@")
	conn := pystr.Strip(strOr(fromConnection))
	match := senderStampRe.FindStringSubmatchIndex(message)
	if match == nil || conn == "" || !relayHandleRe.MatchString(handle) || !relayHandleRe.MatchString(conn) {
		return message
	}
	forms := map[[2]string]string{}
	for i, form := range RemoteTargetForms(roster, localTaken) {
		forms[[2]string{pystr.Lower(roster[i].ConnectionID), pystr.Lower(roster[i].Handle)}] = form
	}
	form, found := forms[[2]string{pystr.Lower(conn), pystr.Lower(handle)}]
	if !found || form == "" {
		form = handle + "@" + conn
	}
	return message[match[2]:match[3]] + form + message[match[6]:match[7]] + message[match[1]:]
}
