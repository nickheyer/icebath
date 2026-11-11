package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Local receipt whose current target row another lane now routes
type MovedReceipt = movedReceipt

// Untracked compression steps a local receipt would adopt with its route entry, nil when nothing is adoptable
func AdoptableCompression(ctx context.Context, conn SQLiteConn, receipt *pyjson.Object) ([]string, *pyjson.Object, error) {
	return adoptableCompression(ctx, conn, receipt)
}

// Physical target of a local receipt once its recorded lineage matches the stored rows
func ValidateLocalLineage(ctx context.Context, conn SQLiteConn, receipt *pyjson.Object) (string, error) {
	return validateLocalLineage(ctx, conn, receipt)
}

// Whether a creation label is one of the local surfaces older builds stored rows under
func LegacyLocalSource(label any) bool {
	text, ok := label.(string)
	return ok && legacyLocalSources[text]
}

// Profile name the rows this store writes carry, empty for the launch profile
func (d *SessionDB) OwnProfileName() string { return d.ownProfileName() }

// Refuses a writer whose epoch is not the current runtime epoch
func RuntimeEpoch(ctx context.Context, conn SQLiteConn, epoch int64) error {
	return runtimeEpoch(ctx, conn, epoch)
}

// Canonical JSON text the runtime ledger stores for a value
func RuntimeJSON(value any) (string, error) { return runtimeJSON(value) }

// Releases every local receipt whose current target a messaging lane holds, returning the released ones
func (d *SessionDB) ReleaseHandedOffReceipts(ctx context.Context, epoch int64, messaging func(source any) bool) ([]MovedReceipt, error) {
	var released []MovedReceipt
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		released = nil
		if err := runtimeEpoch(ctx, conn, epoch); err != nil {
			return err
		}
		found, err := movedReceipts(ctx, conn, nil)
		if err != nil {
			return err
		}
		var lane []movedReceipt
		for _, m := range found {
			if messaging(m.Source) {
				lane = append(lane, m)
			}
		}
		released, err = handOffConversations(ctx, conn, lane)
		return err
	}, nil)
	return released, err
}

// Fences the routes of retired session ids, the expiring ones aging out at the retirement horizon
func RetireRoutes(ctx context.Context, conn SQLiteConn, sessionIDs, expiring []string) error {
	return retireRoutes(ctx, conn, sessionIDs, expiring)
}

// Decoded receipt object of stored text, the error json loads or the shape check raised otherwise
func localReceiptDecoded(raw any) (*pyjson.Object, error) {
	text, ok := raw.(string)
	if !ok {
		b, isBytes := raw.([]byte)
		if !isBytes {
			return nil, &stateTypedError{"TypeError", "the JSON object must be str, bytes or bytearray, not " + pyTypeName(raw)}
		}
		decoded, err := pyJSONDecodeBytes(b)
		if err != nil {
			return nil, err
		}
		text = decoded
	}
	value, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	receipt, isObject := value.(*pyjson.Object)
	if !isObject {
		return nil, &stateTypedError{"ValueError", "invalid receipt"}
	}
	return receipt, nil
}
