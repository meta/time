/*
Copyright (c) Facebook, Inc. and its affiliates.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
Package stats implements statistics collection and reporting.
It is used by server to report internal statistics, such as number of
requests and responses.
*/
package stats

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	ptp "github.com/facebook/time/ptp/protocol"
)

// Stats is a metric collection interface
type Stats interface {
	// Start starts a stat reporter
	// Use this for passive reporters
	Start(monitoringport int)

	// Snapshot the values so they can be reported atomically
	Snapshot()

	// Reset atomically sets all the counters to 0
	Reset()

	// IncSubscription atomically add 1 to the counter
	IncSubscription(t ptp.MessageType)

	// IncRX atomically add 1 to the counter
	IncRX(t ptp.MessageType)

	// IncTX atomically add 1 to the counter
	IncTX(t ptp.MessageType)

	// IncRXSignalingGrant atomically add 1 to the counter
	IncRXSignalingGrant(t ptp.MessageType)

	// IncRXSignalingCancel atomically add 1 to the counter
	IncRXSignalingCancel(t ptp.MessageType)

	// IncTXSignalingGrant atomically add 1 to the counter
	IncTXSignalingGrant(t ptp.MessageType)

	// IncTXSignalingCancel atomically add 1 to the counter
	IncTXSignalingCancel(t ptp.MessageType)

	// IncWorkerSubs atomically add 1 to the counter
	IncWorkerSubs(workerid int)

	// IncReload atomically add 1 to the counter
	IncReload()

	// DecSubscription atomically removes 1 from the counter
	DecSubscription(t ptp.MessageType)

	// DecRX atomically removes 1 from the counter
	DecRX(t ptp.MessageType)

	// DecTX atomically removes 1 from the counter
	DecTX(t ptp.MessageType)

	// DecRXSignalingGrant atomically removes 1 from the counter
	DecRXSignalingGrant(t ptp.MessageType)

	// DecRXSignalingCancel atomically removes 1 from the counter
	DecRXSignalingCancel(t ptp.MessageType)

	// DecTXSignalingGrant atomically removes 1 from the counter
	DecTXSignalingGrant(t ptp.MessageType)

	// DecTXSignalingCancel atomically removes 1 from the counter
	DecTXSignalingCancel(t ptp.MessageType)

	// DecWorkerSubs atomically removes 1 from the counter
	DecWorkerSubs(workerid int)

	// SetMaxWorkerQueue atomically sets worker queue len
	SetMaxWorkerQueue(workerid int, queue int64)

	// SetMaxTXTSAttempts atomically sets number of retries for get latest TX timestamp
	SetMaxTXTSAttempts(workerid int, retries int64)

	// IncTXTSMissing atomically increments the counter when all retries to get latest TX timestamp exceeded
	IncTXTSMissing()

	// SetMinMaxCF atomically sets min/max CF value observed
	SetMinMaxCF(cf int64)

	// SetUTCOffsetSec atomically sets the utcoffset
	SetUTCOffsetSec(utcoffsetSec int64)

	// SetClockAccuracy atomically sets the clock accuracy
	SetClockAccuracy(clockaccuracy int64)

	// SetClockClass atomically sets the clock class
	SetClockClass(clockclass int64)

	// SetDrain atomically sets the drain status
	SetDrain(drain int64)

	// SetRXDrops sets per-worker socket drops
	SetRXDrops(rworker int, drops int64)
}

// relaxedMapInt64 map of per-worker counters
type relaxedMapInt64 struct {
	m []int64
}

// init initializes the underlying array
func (s *relaxedMapInt64) init(cnt int) {
	s.m = make([]int64, cnt)
}

// store sets the counter to v for the given key (assumption that worker runs in 1 thread)
func (s *relaxedMapInt64) store(key int, v int64) {
	atomic.SwapInt64(&s.m[key], v)
}

// load returns the current value of the counter for the given key
func (s *relaxedMapInt64) load(key int) int64 {
	return atomic.LoadInt64(&s.m[key])
}

// reset atomically sets all the counters to 0
func (s *relaxedMapInt64) reset() {
	for k := range s.m {
		atomic.SwapInt64(&(s.m[k]), 0)
	}
}

// syncMapInt64 sync map of PTP messages
type syncMapInt64 struct {
	sync.Mutex
	m map[int]int64
}

// init initializes the underlying map
func (s *syncMapInt64) init() {
	s.m = make(map[int]int64)
}

// keys returns slice of keys of the underlying map
func (s *syncMapInt64) keys() []int {
	s.Lock()
	keys := make([]int, 0, len(s.m))
	for k := range s.m {
		keys = append(keys, k)
	}
	s.Unlock()
	return keys
}

// load gets the value by the key
func (s *syncMapInt64) load(key int) int64 {
	s.Lock()
	defer s.Unlock()
	return s.m[key]
}

// load gets the value by the key
func (s *syncMapInt64) loadAndClear(key int) int64 {
	s.Lock()
	defer s.Unlock()
	v := s.m[key]
	s.m[key] = 0
	return v
}

// inc increments the counter for the given key
func (s *syncMapInt64) inc(key int) {
	s.Lock()
	s.m[key]++
	s.Unlock()
}

// dec decrements the counter for the given key
func (s *syncMapInt64) dec(key int) {
	s.Lock()
	s.m[key]--
	s.Unlock()
}

// store saves the value with the key
func (s *syncMapInt64) store(key int, value int64) {
	s.Lock()
	s.m[key] = value
	s.Unlock()
}

// copy all key-values between maps
func (s *syncMapInt64) copy(dst *syncMapInt64) {
	for _, t := range s.keys() {
		dst.store(t, s.load(t))
	}
}

// copy all key-values between maps
func (s *syncMapInt64) copyAndClear(dst *syncMapInt64) {
	for _, t := range s.keys() {
		dst.store(t, s.loadAndClear(t))
	}
}

// reset stats to 0
func (s *syncMapInt64) reset() {
	s.Lock()
	for t := range s.m {
		s.m[t] = 0
	}

	s.Unlock()
}

type counters struct {
	rx                syncMapInt64
	rxSignalingGrant  syncMapInt64
	rxSignalingCancel syncMapInt64
	subscriptions     syncMapInt64
	tx                syncMapInt64
	txSignalingGrant  syncMapInt64
	txSignalingCancel syncMapInt64
	txtsattempts      syncMapInt64
	workerQueue       syncMapInt64
	workerSubs        syncMapInt64
	rxDrops           relaxedMapInt64
	utcoffsetSec      int64
	clockaccuracy     int64
	clockclass        int64
	drain             int64
	reload            int64
	txtsMissing       int64
	minMaxCF          int64
}

func (c *counters) init(rworkers int) {
	c.subscriptions.init()
	c.rx.init()
	c.tx.init()
	c.rxSignalingGrant.init()
	c.rxSignalingCancel.init()
	c.txSignalingGrant.init()
	c.txSignalingCancel.init()
	c.workerQueue.init()
	c.workerSubs.init()
	c.txtsattempts.init()
	c.rxDrops.init(rworkers)
}

func (c *counters) reset() {
	c.subscriptions.reset()
	c.rx.reset()
	c.tx.reset()
	c.rxSignalingGrant.reset()
	c.rxSignalingCancel.reset()
	c.txSignalingGrant.reset()
	c.txSignalingCancel.reset()
	c.workerQueue.reset()
	c.workerSubs.reset()
	c.txtsattempts.reset()
	c.rxDrops.reset()
	c.utcoffsetSec = 0
	c.clockaccuracy = 0
	c.clockclass = 0
	c.drain = 0
	c.reload = 0
	c.txtsMissing = 0
	c.minMaxCF = 0
}

// toMap converts counters to a map
func (c *counters) toMap() (export map[string]int64) {
	res := make(map[string]int64)

	for _, t := range c.subscriptions.keys() {
		c := c.subscriptions.load(t)
		mt := strings.ToLower(ptp.MessageType(t).String())
		res[fmt.Sprintf("subscriptions.%s", mt)] = c
	}

	for _, t := range c.rx.keys() {
		c := c.rx.load(t)
		mt := strings.ToLower(ptp.MessageType(t).String())
		res[fmt.Sprintf("rx.%s", mt)] = c
	}

	for _, t := range c.tx.keys() {
		c := c.tx.load(t)
		mt := strings.ToLower(ptp.MessageType(t).String())
		res[fmt.Sprintf("tx.%s", mt)] = c
	}

	for _, t := range c.rxSignalingGrant.keys() {
		c := c.rxSignalingGrant.load(t)
		mt := strings.ToLower(ptp.MessageType(t).String())
		res[fmt.Sprintf("rx.signaling.grant.%s", mt)] = c
	}

	for _, t := range c.rxSignalingCancel.keys() {
		c := c.rxSignalingCancel.load(t)
		mt := strings.ToLower(ptp.MessageType(t).String())
		res[fmt.Sprintf("rx.signaling.cancel.%s", mt)] = c
	}

	for _, t := range c.txSignalingGrant.keys() {
		c := c.txSignalingGrant.load(t)
		mt := strings.ToLower(ptp.MessageType(t).String())
		res[fmt.Sprintf("tx.signaling.grant.%s", mt)] = c
	}

	for _, t := range c.txSignalingCancel.keys() {
		c := c.txSignalingCancel.load(t)
		mt := strings.ToLower(ptp.MessageType(t).String())
		res[fmt.Sprintf("tx.signaling.cancel.%s", mt)] = c
	}

	for _, t := range c.workerQueue.keys() {
		c := c.workerQueue.load(t)
		res[fmt.Sprintf("worker.%d.queue", t)] = c
	}

	for _, t := range c.workerSubs.keys() {
		c := c.workerSubs.load(t)
		res[fmt.Sprintf("worker.%d.subscriptions", t)] = c
	}

	for _, t := range c.txtsattempts.keys() {
		c := c.txtsattempts.load(t)
		res[fmt.Sprintf("worker.%d.txtsattempts", t)] = c
	}

	var drops int64
	for k := range c.rxDrops.m {
		drops += c.rxDrops.load(k)
	}
	res["rxdrops"] = drops

	res["utcoffset_sec"] = c.utcoffsetSec
	res["clockaccuracy"] = c.clockaccuracy
	res["clockclass"] = c.clockclass
	res["drain"] = c.drain
	res["reload"] = c.reload
	res["txts.missing"] = c.txtsMissing
	res["min_max_cf"] = c.minMaxCF

	return res
}
