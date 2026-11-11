package gatewayrun

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
)

// Seconds a recovered notice waits, dropped when the store degrades again meanwhile
const noticeRepeatSeconds = 3600.0

// Routes cron store transitions from the ticker onto the gateway loop and announces each outage once
func installCronStoreNotices(ctx context.Context, r *GatewayRunner, loop gateway.EventLoop) {
	ctx = context.WithoutCancel(ctx)
	var mu sync.Mutex
	announced := map[string]bool{}
	pending := map[string]gateway.LoopTimer{}
	sending := map[string]*sync.Mutex{}
	deliver := func(c context.Context, event string, record *cron.StoreDegraded) error {
		mu.Lock()
		lock := sending[record.Store]
		if lock == nil {
			lock = &sync.Mutex{}
			sending[record.Store] = lock
		}
		mu.Unlock()
		lock.Lock()
		defer lock.Unlock()
		degraded := cron.DegradedRecord(record.Store) != nil
		mu.Lock()
		if event == "unwritable" && !degraded {
			delete(announced, record.Store)
			if handle, ok := pending[record.Store]; ok {
				delete(pending, record.Store)
				handle.Cancel()
			}
			mu.Unlock()
			return nil
		}
		if event == "recovered" {
			if degraded || !announced[record.Store] {
				mu.Unlock()
				return nil
			}
			delete(announced, record.Store)
		}
		mu.Unlock()
		return sendCronStoreNotice(c, r, event, record)
	}
	send := func(event string, record *cron.StoreDegraded) {
		task := spawnGatewayTask(ctx, "", func(c context.Context) (any, error) { return nil, deliver(c, event, record) })
		task.addDoneCallback(func(done *gatewayFuture) { logNoticeFailure(done, event, record.Store) })
	}
	sendRecovered := func(record *cron.StoreDegraded) {
		mu.Lock()
		delete(pending, record.Store)
		owed := announced[record.Store]
		mu.Unlock()
		if owed {
			send("recovered", record)
		}
	}
	onLoop := func(event string, record *cron.StoreDegraded) {
		store := record.Store
		mu.Lock()
		defer mu.Unlock()
		if event == "unwritable" {
			if handle, ok := pending[store]; ok {
				delete(pending, store)
				handle.Cancel()
			} else if !announced[store] {
				announced[store] = true
				send(event, record)
			}
			return
		}
		if _, waiting := pending[store]; announced[store] && !waiting {
			handle, err := loop.CallLater(time.Duration(noticeRepeatSeconds*float64(time.Second)), func() { sendRecovered(record) })
			if err != nil {
				panic(err)
			}
			pending[store] = handle
		}
	}
	onTransition := func(event string, record *cron.StoreDegraded) {
		if err := loop.CallSoonThreadsafe(func() { onLoop(event, record) }); err != nil {
			panic(err)
		}
	}
	cron.SetStoreTransitionListener(onTransition)
	for _, record := range cron.DegradedRecords() {
		onTransition("unwritable", record)
	}
}

// Logs a failed notice send, a cancelled one passing silently
func logNoticeFailure(done *gatewayFuture, event, store string) {
	if done.cancelled() {
		return
	}
	if _, err := done.wait(context.Background()); err != nil {
		runLog().Warn(fmt.Sprintf("Cron store %s notice for %s failed", event, store), "exc_info", err)
	}
}

// Posts one unwritable or recovered notice to the home channels of the store's profile
func sendCronStoreNotice(ctx context.Context, r *GatewayRunner, event string, record *cron.StoreDegraded) error {
	fields := record.NoticeFields()
	key := "gateway.cron_store.recovered"
	if event == "unwritable" {
		key = "gateway.cron_store.unwritable"
	}
	runLog().Info(fmt.Sprintf("Broadcasting cron store %s notice for %s", event, record.Store))
	for _, target := range r.servedHomeChannelTransports(ctx) {
		servedHome := ""
		if target.profile != nil && r.servedProfileHomes != nil {
			servedHome, _ = r.servedProfileHomes.Get(*target.profile)
		}
		owner := servedHome
		if owner == "" {
			owner = home.RoutingProcess()
		}
		if cron.StoreKey(owner) != record.Store {
			continue
		}
		scoped := ctx
		if servedHome != "" {
			scoped = gateway.ProfileRuntimeScope(ctx, servedHome, true)
		}
		message := i18n.T(scoped, key, "", fields)
		t := target
		if _, err := config.PresentNotification(scoped, func(c context.Context) error {
			r.sendHomeChannelMessage(c, t.platform, t.home, t.transport, message, "Cron store notice failed for %s:%s: %s")
			return nil
		}, string(t.platform), true, nil); err != nil {
			return err
		}
	}
	return nil
}
