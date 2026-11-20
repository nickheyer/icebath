package cron

import "time"

// Durable delivery queue of a home, the queue file the test override points elsewhere
func DeliveryQueueOf(home string) *DeliveryQueue {
	return openDeliveryQueue(openCronStore(home, time.Now))
}
