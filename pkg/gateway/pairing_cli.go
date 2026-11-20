package gateway

import (
	"context"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Left aligns text in a column of the given width counted in code points
func padColumn(text string, width int) string {
	if n := utf8.RuneCountInString(text); n < width {
		return text + strings.Repeat(" ", width-n)
	}
	return text
}

// Runs the pairing subcommands list, approve, revoke and clear-pending over the store of the bound home
func PairingCommand(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	store, err := NewPairingStore(ctx, "")
	if err != nil {
		return 0, err
	}
	return pairingCommand(ctx, store, ns, w)
}

// Dispatches one pairing subcommand against a store
func pairingCommand(ctx context.Context, store *PairingStore, ns map[string]any, w io.Writer) (int, error) {
	text := func(key string) string {
		value, _ := ns[key].(string)
		return value
	}
	switch text("pairing_action") {
	case "list":
		return 0, pairingCmdList(ctx, store, w)
	case "approve":
		return 0, pairingCmdApprove(ctx, store, text("platform"), text("code"), w)
	case "revoke":
		return 0, pairingCmdRevoke(ctx, store, text("platform"), text("user_id"), w)
	case "clear-pending":
		return 0, pairingCmdClearPending(ctx, store, w)
	}
	fmt.Fprintln(w, "Usage: icebath pairing {list|approve|revoke|clear-pending}")
	fmt.Fprintln(w, "Run 'icebath pairing --help' for details.")
	return 0, nil
}

// Text of a stored value, the fallback when the value is falsy
func pairingText(value any, fallback string) string {
	if !pyrepr.Truthy(value) {
		return fallback
	}
	return pyrepr.Str(value)
}

// Lists every pending request and approved user
func pairingCmdList(ctx context.Context, store *PairingStore, w io.Writer) error {
	pending, err := store.ListPending(ctx, "")
	if err != nil {
		return err
	}
	approved, err := store.ListApproved("")
	if err != nil {
		return err
	}
	if len(pending) == 0 && len(approved) == 0 {
		fmt.Fprintln(w, "No pairing data found. No one has tried to pair yet~")
		return nil
	}
	if len(pending) > 0 {
		fmt.Fprintf(w, "\n  Pending Pairing Requests (%d):\n", len(pending))
		fmt.Fprintf(w, "  %s %s %s %s %s\n", padColumn("Platform", 12), padColumn("Request ID", 18), padColumn("User ID", 20), padColumn("Name", 20), "Age")
		fmt.Fprintf(w, "  %s %s %s %s %s\n", padColumn("--------", 12), padColumn("----------", 18), padColumn("-------", 20), padColumn("----", 20), "---")
		for _, p := range pending {
			fmt.Fprintf(w, "  %s %s %s %s %dm ago\n", padColumn(p.Platform, 12), padColumn(pairingText(p.RequestID, "-"), 18),
				padColumn(pyrepr.Str(p.UserID), 20), padColumn(pairingText(p.UserName, ""), 20), p.AgeMinutes)
		}
		fmt.Fprintln(w, "\n  Approve with: icebath pairing approve <platform> <request-id>")
		fmt.Fprintln(w, "  The code the bot DM'd the user also works if they relay it.")
	} else {
		fmt.Fprintln(w, "\n  No pending pairing requests.")
	}
	if len(approved) > 0 {
		fmt.Fprintf(w, "\n  Approved Users (%d):\n", len(approved))
		fmt.Fprintf(w, "  %s %s %s\n", padColumn("Platform", 12), padColumn("User ID", 20), padColumn("Name", 20))
		fmt.Fprintf(w, "  %s %s %s\n", padColumn("--------", 12), padColumn("-------", 20), padColumn("----", 20))
		for _, a := range approved {
			fmt.Fprintf(w, "  %s %s %s\n", padColumn(pyrepr.Str(a.GetOr("platform", nil)), 12), padColumn(pyrepr.Str(a.GetOr("user_id", nil)), 20),
				padColumn(pairingText(a.GetOr("user_name", nil), ""), 20))
		}
	} else {
		fmt.Fprintln(w, "\n  No approved users.")
	}
	fmt.Fprintln(w)
	return nil
}

// Approves a request id from the pending list or a code the bot sent the user
func pairingCmdApprove(ctx context.Context, store *PairingStore, platform, code string, w io.Writer) error {
	platform = pystr.Strip(strings.ToLower(platform))
	code = pystr.Strip(code)
	var result *PairingApproval
	var err error
	if LooksLikePairingRequestID(code) {
		result, err = store.ApproveRequest(ctx, platform, code)
	} else {
		result, err = store.ApproveCode(ctx, platform, strings.ToUpper(code))
	}
	if err != nil {
		return err
	}
	if result != nil {
		display := result.UserID
		if result.UserName != "" {
			display = fmt.Sprintf("%s (%s)", result.UserName, result.UserID)
		}
		fmt.Fprintf(w, "\n  Approved! User %s on %s can now use the bot~\n", display, platform)
		fmt.Fprint(w, "  They'll be recognized automatically on their next message.\n\n")
		return nil
	}
	lockedOut, err := store.isLockedOut(platform)
	if err != nil {
		return err
	}
	if lockedOut {
		limits, err := store.loadJSON(store.rateLimitPath())
		if err != nil {
			return err
		}
		lockoutUntil, err := pairingLimit(limits, "_lockout:"+platform)
		if err != nil {
			return err
		}
		mins := max(0, int(lockoutUntil-pairingNow())) / 60
		fmt.Fprintf(w, "\n  Platform '%s' is locked out after too many failed approval attempts.\n", platform)
		fmt.Fprintf(w, "  Lockout clears in ~%d minute(s).\n", mins)
		fmt.Fprintf(w, "  To reset sooner, delete the '_lockout:%s' entry from ~/.icebath/platforms/pairing/_rate_limits.json\n\n", platform)
		return nil
	}
	fmt.Fprintf(w, "\n  Pairing request or code '%s' not found or expired for platform '%s'.\n", code, platform)
	fmt.Fprint(w, "  Run 'icebath pairing list' to see pending requests.\n\n")
	return nil
}

// Revokes the access of a user on a platform
func pairingCmdRevoke(ctx context.Context, store *PairingStore, platform, userID string, w io.Writer) error {
	platform = pystr.Strip(strings.ToLower(platform))
	found, err := store.Revoke(ctx, platform, userID)
	if err != nil {
		return err
	}
	if found {
		fmt.Fprintf(w, "\n  Revoked access for user %s on %s.\n\n", userID, platform)
	} else {
		fmt.Fprintf(w, "\n  User %s not found in approved list for %s.\n\n", userID, platform)
	}
	return nil
}

// Clears every pending pairing request
func pairingCmdClearPending(ctx context.Context, store *PairingStore, w io.Writer) error {
	count, err := store.ClearPending(ctx, "")
	if err != nil {
		return err
	}
	if count > 0 {
		fmt.Fprintf(w, "\n  Cleared %d pending pairing request(s).\n\n", count)
	} else {
		fmt.Fprint(w, "\n  No pending requests to clear.\n\n")
	}
	return nil
}
