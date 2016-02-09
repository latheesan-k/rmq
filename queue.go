package rmq

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/adjust/uniuri"
	"gopkg.in/redis.v3"
)

const (
	connectionsKey                   = "rmq::connections"                                           // Set of connection names
	connectionHeartbeatTemplate      = "rmq::connection::{connection}::heartbeat"                   // expires after {connection} died
	connectionQueuesTemplate         = "rmq::connection::{connection}::queues"                      // Set of queues consumers of {connection} are consuming
	connectionQueueConsumersTemplate = "rmq::connection::{connection}::queue::[{queue}]::consumers" // Set of all consumers from {connection} consuming from {queue}
	connectionQueueUnackedTemplate   = "rmq::connection::{connection}::queue::[{queue}]::unacked"   // List of deliveries consumers of {connection} are currently consuming

	queuesKey             = "rmq::queues"                     // Set of all open queues
	queueReadyTemplate    = "rmq::queue::[{queue}]::ready"    // List of deliveries in that {queue} (right is first and oldest, left is last and youngest)
	queueRejectedTemplate = "rmq::queue::[{queue}]::rejected" // List of rejected deliveries from that {queue}

	phConnection = "{connection}" // connection name
	phQueue      = "{queue}"      // queue name
	phConsumer   = "{consumer}"   // consumer name (consisting of tag and token)
)

type Queue interface {
	SetPublishBufferSize(size int, pollDuration time.Duration)
	Publish(payload string) bool
	PublishBytes(payload []byte) bool
	SetPushQueue(pushQueue Queue)
	StartConsuming(prefetchLimit int, pollDuration time.Duration) bool
	StopConsuming() bool
	AddConsumer(tag string, consumer Consumer) string
	AddBatchConsumer(tag string, batchSize int, consumer BatchConsumer) string
	PurgeReady() bool
	PurgeRejected() bool
	ReturnRejected(count int) int
	ReturnAllRejected() int
	Close() bool
}

type redisQueue struct {
	name           string
	connectionName string
	queuesKey      string // key to list of queues consumed by this connection
	consumersKey   string // key to set of consumers using this connection
	readyKey       string // key to list of ready deliveries
	rejectedKey    string // key to list of rejected deliveries
	unackedKey     string // key to list of currently consuming deliveries
	pushKey        string // key to list of pushed deliveries
	redisClient    *redis.Client

	consumeChan         chan Delivery // nil for publish channels, not nil for consuming channels
	prefetchLimit       int           // max number of prefetched deliveries number of unacked can go up to prefetchLimit + numConsumers
	consumePollDuration time.Duration // how long to wait between polling on empty consumeChan
	consumingStopped    bool

	publishChan         chan string     // buffered publishes go here if exists
	publishMutex        *sync.RWMutex   // protects publishChan and SetPublishBufferSize
	publishWg           *sync.WaitGroup // wait for buffered publishes to complete
	publishPollDuration time.Duration   // how long to wait between polling on empty publishChan
}

func newQueue(name, connectionName, queuesKey string, redisClient *redis.Client) *redisQueue {
	consumersKey := strings.Replace(connectionQueueConsumersTemplate, phConnection, connectionName, 1)
	consumersKey = strings.Replace(consumersKey, phQueue, name, 1)

	readyKey := strings.Replace(queueReadyTemplate, phQueue, name, 1)
	rejectedKey := strings.Replace(queueRejectedTemplate, phQueue, name, 1)

	unackedKey := strings.Replace(connectionQueueUnackedTemplate, phConnection, connectionName, 1)
	unackedKey = strings.Replace(unackedKey, phQueue, name, 1)

	queue := &redisQueue{
		name:           name,
		connectionName: connectionName,
		queuesKey:      queuesKey,
		consumersKey:   consumersKey,
		readyKey:       readyKey,
		rejectedKey:    rejectedKey,
		unackedKey:     unackedKey,
		redisClient:    redisClient,
		publishMutex:   &sync.RWMutex{},
	}
	return queue
}

func (queue *redisQueue) String() string {
	return fmt.Sprintf("[%s conn:%s]", queue.name, queue.connectionName)
}

// SetPublishBufferSize allows you to enable and disable buffering of deliveries to be published
// change from 0 to 10 to enable buffering of up to 10 published deliveries
// change from 10 to 0 to disable buffering again. blocks until buffer is processed
// changing from 10 to 20 disables buffering (blocking) and then enables it again
func (queue *redisQueue) SetPublishBufferSize(size int, pollDuration time.Duration) {
	fmt.Printf("%s SetPublishBufferSize enter\n", time.Now())
	queue.publishMutex.Lock() // make thread safe
	fmt.Printf("%s SetPublishBufferSize locked\n", time.Now())
	defer queue.publishMutex.Unlock()

	if cap(queue.publishChan) == size {
		return // nothing to do
	}

	if queue.publishChan != nil { // stop buffering
		close(queue.publishChan)
		queue.publishWg.Wait()
		fmt.Printf("%s SetPublishBufferSize waited\n", time.Now())
		queue.publishChan = nil
		queue.publishWg = nil
	}

	if size > 0 { // start buffering
		queue.publishPollDuration = pollDuration
		queue.publishChan = make(chan string, size)
		queue.publishWg = &sync.WaitGroup{}
		queue.publishWg.Add(1)
		go queue.publish()
	}
}

func (queue *redisQueue) publish() {
	batch := []string{}
	batchLen := 0
	for {
		select {
		case payload, ok := <-queue.publishChan:
			if ok { // got payload
				if len(batch) <= batchLen {
					batch = append(batch, payload)
				} else {
					batch[batchLen] = payload
				}
				batchLen++

			} else { // channel closed
				if batchLen > 0 {
					if redisErrIsNil(queue.redisClient.LPush(queue.readyKey, batch[:batchLen]...)) {
						log.Printf("failed to publish last batch %q", batch[:batchLen])
					}
				}
				queue.publishWg.Done()
				return
			}

		default: // channel empty
			if batchLen > 0 { // send batch
				if redisErrIsNil(queue.redisClient.LPush(queue.readyKey, batch[:batchLen]...)) {
					log.Printf("failed to publish batch %q", batch[:batchLen])
				}
				batchLen = 0

			} else { // nothing to publish
				time.Sleep(queue.publishPollDuration)
			}
		}
	}
}

// Publish adds a delivery with the given payload to the queue
func (queue *redisQueue) Publish(payload string) bool {
	// debug(fmt.Sprintf("publish %s %s", payload, queue)) // COMMENTOUT
	queue.publishMutex.RLock()
	defer queue.publishMutex.RUnlock()

	if queue.publishChan != nil { // publish buffered to channel
		queue.publishChan <- payload
		return true
	}

	return !redisErrIsNil(queue.redisClient.LPush(queue.readyKey, payload))
}

// PublishBytes just casts the bytes and calls Publish
func (queue *redisQueue) PublishBytes(payload []byte) bool {
	return queue.Publish(string(payload))
}

// PurgeReady removes all ready deliveries from the queue and returns the number of purged deliveries
func (queue *redisQueue) PurgeReady() bool {
	result := queue.redisClient.Del(queue.readyKey)
	if redisErrIsNil(result) {
		return false
	}
	return result.Val() > 0
}

// PurgeRejected removes all rejected deliveries from the queue and returns the number of purged deliveries
func (queue *redisQueue) PurgeRejected() bool {
	result := queue.redisClient.Del(queue.rejectedKey)
	if redisErrIsNil(result) {
		return false
	}
	return result.Val() > 0
}

// Close purges and removes the queue from the list of queues
func (queue *redisQueue) Close() bool {
	queue.PurgeRejected()
	queue.PurgeReady()
	result := queue.redisClient.SRem(queuesKey, queue.name)
	if redisErrIsNil(result) {
		return false
	}
	return result.Val() > 0
}

func (queue *redisQueue) ReadyCount() int {
	result := queue.redisClient.LLen(queue.readyKey)
	if redisErrIsNil(result) {
		return 0
	}
	return int(result.Val())
}

func (queue *redisQueue) UnackedCount() int {
	result := queue.redisClient.LLen(queue.unackedKey)
	if redisErrIsNil(result) {
		return 0
	}
	return int(result.Val())
}

func (queue *redisQueue) RejectedCount() int {
	result := queue.redisClient.LLen(queue.rejectedKey)
	if redisErrIsNil(result) {
		return 0
	}
	return int(result.Val())
}

// ReturnAllUnacked moves all unacked deliveries back to the ready
// queue and deletes the unacked key afterwards, returns number of returned
// deliveries
func (queue *redisQueue) ReturnAllUnacked() int {
	result := queue.redisClient.LLen(queue.unackedKey)
	if redisErrIsNil(result) {
		return 0
	}

	unackedCount := int(result.Val())
	for i := 0; i < unackedCount; i++ {
		if redisErrIsNil(queue.redisClient.RPopLPush(queue.unackedKey, queue.readyKey)) {
			return i
		}
		// debug(fmt.Sprintf("rmq queue returned unacked delivery %s %s", result.Val(), queue.readyKey)) // COMMENTOUT
	}

	return unackedCount
}

// ReturnAllRejected moves all rejected deliveries back to the ready
// list and returns the number of returned deliveries
func (queue *redisQueue) ReturnAllRejected() int {
	result := queue.redisClient.LLen(queue.rejectedKey)
	if redisErrIsNil(result) {
		return 0
	}

	rejectedCount := int(result.Val())
	return queue.ReturnRejected(rejectedCount)
}

// ReturnRejected tries to return count rejected deliveries back to
// the ready list and returns the number of returned deliveries
func (queue *redisQueue) ReturnRejected(count int) int {
	if count == 0 {
		return 0
	}

	for i := 0; i < count; i++ {
		result := queue.redisClient.RPopLPush(queue.rejectedKey, queue.readyKey)
		if redisErrIsNil(result) {
			return i
		}
		// debug(fmt.Sprintf("rmq queue returned rejected delivery %s %s", result.Val(), queue.readyKey)) // COMMENTOUT
	}

	return count
}

// CloseInConnection closes the queue in the associated connection by removing all related keys
func (queue *redisQueue) CloseInConnection() {
	redisErrIsNil(queue.redisClient.Del(queue.unackedKey))
	redisErrIsNil(queue.redisClient.Del(queue.consumersKey))
	redisErrIsNil(queue.redisClient.SRem(queue.queuesKey, queue.name))
}

func (queue *redisQueue) SetPushQueue(pushQueue Queue) {
	redisPushQueue, ok := pushQueue.(*redisQueue)
	if !ok {
		return
	}

	queue.pushKey = redisPushQueue.readyKey
}

// StartConsuming starts consuming into a channel of size prefetchLimit
// must be called before consumers can be added!
// pollDuration is the duration the queue sleeps before checking for new deliveries
func (queue *redisQueue) StartConsuming(prefetchLimit int, pollDuration time.Duration) bool {
	if queue.consumeChan != nil {
		return false // already consuming
	}

	// add queue to list of queues consumed on this connection
	if redisErrIsNil(queue.redisClient.SAdd(queue.queuesKey, queue.name)) {
		log.Panicf("rmq queue failed to start consuming %s", queue)
	}

	queue.prefetchLimit = prefetchLimit
	queue.consumePollDuration = pollDuration
	queue.consumeChan = make(chan Delivery, prefetchLimit)
	// log.Printf("rmq queue started consuming %s %d %s", queue, prefetchLimit, pollDuration)
	go queue.consume()
	return true
}

func (queue *redisQueue) StopConsuming() bool {
	if queue.consumeChan == nil || queue.consumingStopped {
		return false // not consuming or already stopped
	}

	queue.consumingStopped = true
	return true
}

// AddConsumer adds a consumer to the queue and returns its internal name
// panics if StartConsuming wasn't called before!
func (queue *redisQueue) AddConsumer(tag string, consumer Consumer) string {
	name := queue.addConsumer(tag)
	go queue.consumerConsume(consumer)
	return name
}

// AddBatchConsumer is similar to AddConsumer, but for batches of deliveries
func (queue *redisQueue) AddBatchConsumer(tag string, batchSize int, consumer BatchConsumer) string {
	name := queue.addConsumer(tag)
	go queue.consumerBatchConsume(batchSize, consumer)
	return name
}

func (queue *redisQueue) GetConsumers() []string {
	result := queue.redisClient.SMembers(queue.consumersKey)
	if redisErrIsNil(result) {
		return []string{}
	}
	return result.Val()
}

func (queue *redisQueue) RemoveConsumer(name string) bool {
	result := queue.redisClient.SRem(queue.consumersKey, name)
	if redisErrIsNil(result) {
		return false
	}
	return result.Val() > 0
}

func (queue *redisQueue) addConsumer(tag string) string {
	if queue.consumeChan == nil {
		log.Panicf("rmq queue failed to add consumer, call StartConsuming first! %s", queue)
	}

	name := fmt.Sprintf("%s-%s", tag, uniuri.NewLen(6))

	// add consumer to list of consumers of this queue
	if redisErrIsNil(queue.redisClient.SAdd(queue.consumersKey, name)) {
		log.Panicf("rmq queue failed to add consumer %s %s", queue, tag)
	}

	// log.Printf("rmq queue added consumer %s %s", queue, name)
	return name
}

func (queue *redisQueue) RemoveAllConsumers() int {
	result := queue.redisClient.Del(queue.consumersKey)
	if redisErrIsNil(result) {
		return 0
	}
	return int(result.Val())
}

func (queue *redisQueue) consume() {
	for {
		batchSize := queue.batchSize()
		wantMore := queue.consumeBatch(batchSize)

		if !wantMore {
			time.Sleep(queue.consumePollDuration)
		}

		if queue.consumingStopped {
			// log.Printf("rmq queue stopped consuming %s", queue)
			return
		}
	}
}

func (queue *redisQueue) batchSize() int {
	prefetchCount := len(queue.consumeChan)
	prefetchLimit := queue.prefetchLimit - prefetchCount
	// TODO: ignore ready count here and just return prefetchLimit?
	if readyCount := queue.ReadyCount(); readyCount < prefetchLimit {
		return readyCount
	}
	return prefetchLimit
}

// consumeBatch tries to read batchSize deliveries, returns true if any and all were consumed
func (queue *redisQueue) consumeBatch(batchSize int) bool {
	if batchSize == 0 {
		return false
	}

	for i := 0; i < batchSize; i++ {
		result := queue.redisClient.RPopLPush(queue.readyKey, queue.unackedKey)
		if redisErrIsNil(result) {
			// debug(fmt.Sprintf("rmq queue consumed last batch %s %d", queue, i)) // COMMENTOUT
			return false
		}

		// debug(fmt.Sprintf("consume %d/%d %s %s", i, batchSize, result.Val(), queue)) // COMMENTOUT
		queue.consumeChan <- newDelivery(result.Val(), queue.unackedKey, queue.rejectedKey, queue.pushKey, queue.redisClient)
	}

	// debug(fmt.Sprintf("rmq queue consumed batch %s %d", queue, batchSize)) // COMMENTOUT
	return true
}

func (queue *redisQueue) consumerConsume(consumer Consumer) {
	for delivery := range queue.consumeChan {
		// debug(fmt.Sprintf("consumer consume %s %s", delivery, consumer)) // COMMENTOUT
		consumer.Consume(delivery)
	}
}

func (queue *redisQueue) consumerBatchConsume(batchSize int, consumer BatchConsumer) {
	batch := []Delivery{}
	waitUntil := time.Now().UTC().Add(time.Second)

	for delivery := range queue.consumeChan {
		batch = append(batch, delivery)
		now := time.Now().UTC()
		// debug(fmt.Sprintf("batch consume added delivery %d", len(batch))) // COMMENTOUT

		if len(batch) < batchSize && now.Before(waitUntil) {
			// debug(fmt.Sprintf("batch consume wait %d < %d", len(batch), batchSize)) // COMMENTOUT
			continue
		}

		// debug(fmt.Sprintf("batch consume consume %d", len(batch))) // COMMENTOUT
		consumer.Consume(batch)

		batch = []Delivery{}
		waitUntil = time.Now().UTC().Add(time.Second)
	}
}

// redisErrIsNil returns false if there is no error, true if the result error is nil and panics if there's another error
func redisErrIsNil(result redis.Cmder) bool {
	switch result.Err() {
	case nil:
		return false
	case redis.Nil:
		return true
	default:
		log.Panicf("rmq redis error is not nil %s", result.Err())
		return false
	}
}

func debug(message string) {
	// log.Printf("rmq debug: %s", message) // COMMENTOUT
}
