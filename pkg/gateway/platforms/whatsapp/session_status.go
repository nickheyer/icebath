package whatsapp

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Session directory the adapter opens, whether its whatsmeow store exists and whether a linked device is stored in it
func SessionStatus(ctx context.Context, extra map[string]any) (dir string, present, paired bool, err error) {
	dir = sessionDir(ctx, extra)
	path := filepath.Join(dir, bridgeSessionDBName)
	if st, statErr := os.Stat(path); statErr != nil || !st.Mode().IsRegular() {
		return dir, false, false, nil
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return dir, true, false, err
	}
	defer db.Close()
	device, err := sqlstore.NewWithDB(db, "sqlite", waLog.Noop).GetFirstDevice(ctx)
	if err != nil {
		return dir, true, false, err
	}
	return dir, true, device.ID != nil, nil
}
