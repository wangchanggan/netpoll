// Copyright 2022 CloudWeGo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mux

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/cloudwego/netpoll"
	"github.com/cloudwego/netpoll/internal/runner"
)

/* DOC:
 * ShardQueue uses the netpoll's nocopy API to merge and send data.
 * The Data Flush is passively triggered by ShardQueue.Add and does not require user operations.
 * If there is an error in the data transmission, the connection will be closed.
 *
 * ShardQueue.Add: add the data to be sent.
 * NewShardQueue: create a queue with netpoll.Connection.
 * ShardSize: the recommended number of shards is 32.
 */
var ShardSize int

func init() {
	ShardSize = runtime.GOMAXPROCS(0)
}

// NewShardQueue .
func NewShardQueue(size int, conn netpoll.Connection) (queue *ShardQueue) {
	queue = &ShardQueue{
		conn:    conn,
		size:    int32(size),
		getters: make([][]WriterGetter, size),
		swap:    make([]WriterGetter, 0, 64),
		locks:   make([]int32, size),
	}
	for i := range queue.getters {
		queue.getters[i] = make([]WriterGetter, 0, 64)
	}
	queue.list = make([]int32, size)
	return queue
}

// WriterGetter is used to get a netpoll.Writer.
type WriterGetter func() (buf netpoll.Writer, isNil bool)

// ShardQueue uses the netpoll's nocopy API to merge and send data.
// The Data Flush is passively triggered by ShardQueue.Add and does not require user operations.
// If there is an error in the data transmission, the connection will be closed.
// ShardQueue.Add: add the data to be sent.
type ShardQueue struct {
	conn      netpoll.Connection
	idx, size int32
	getters   [][]WriterGetter // len(getters) = size
	swap      []WriterGetter   // use for swap
	locks     []int32          // len(locks) = size
	queueTrigger
}

const (
	// queueTrigger state
	active  = 0
	closing = 1
	closed  = 2
)

// here for trigger
type queueTrigger struct {
	trigger  int32
	state    int32 // 0: active, 1: closing, 2: closed
	runNum   int32
	w, r     int32      // ptr of list
	list     []int32    // record the triggered shard
	listLock sync.Mutex // list total lock
}

// Add adds to q.getters[shard]
func (q *ShardQueue) Add(gts ...WriterGetter) {
	if atomic.LoadInt32(&q.state) != active {
		return
	}
	shard := atomic.AddInt32(&q.idx, 1) % q.size
	q.lock(shard)
	trigger := len(q.getters[shard]) == 0
	q.getters[shard] = append(q.getters[shard], gts...)
	q.unlock(shard)
	if trigger {
		q.triggering(shard)
	}
}

func (q *ShardQueue) Close() error {
	if !atomic.CompareAndSwapInt32(&q.state, active, closing) {
		return fmt.Errorf("shardQueue has been closed")
	}
	// wait for all tasks finished
	for atomic.LoadInt32(&q.state) != closed {
		if atomic.LoadInt32(&q.trigger) == 0 {
			atomic.StoreInt32(&q.state, closed)
			return nil
		}
		runtime.Gosched()
	}
	return nil
}

// triggering shard.
func (q *ShardQueue) triggering(shard int32) {
	q.listLock.Lock()
	q.w = (q.w + 1) % q.size
	q.list[q.w] = shard
	q.listLock.Unlock()

	if atomic.AddInt32(&q.trigger, 1) > 1 {
		return
	}
	q.foreach()
}

// foreach swap r & w. It's not concurrency safe.
func (q *ShardQueue) foreach() {
	if atomic.AddInt32(&q.runNum, 1) > 1 {
		return
	}
	runner.RunTask(nil, func() {
		var negNum int32 // is negative number of triggerNum
		for triggerNum := atomic.LoadInt32(&q.trigger); triggerNum > 0; {
			q.r = (q.r + 1) % q.size
			shared := q.list[q.r]

			// lock & swap
			q.lock(shared)
			tmp := q.getters[shared]
			q.getters[shared] = q.swap[:0]
			q.swap = tmp
			q.unlock(shared)

			// deal
			q.deal(q.swap)
			negNum--
			if triggerNum+negNum == 0 {
				triggerNum = atomic.AddInt32(&q.trigger, negNum)
				negNum = 0
			}
		}
		q.flush()

		// quit & check again
		atomic.StoreInt32(&q.runNum, 0)
		if atomic.LoadInt32(&q.trigger) > 0 {
			q.foreach()
			return
		}
		// if state is closing, change it to closed
		atomic.CompareAndSwapInt32(&q.state, closing, closed)
	})
}

// deal is used to get deal of netpoll.Writer.
func (q *ShardQueue) deal(gts []WriterGetter) {
	writer := q.conn.Writer()
	for _, gt := range gts {
		buf, isNil := gt()
		if !isNil {
			err := writer.Append(buf)
			if err != nil {
				q.conn.Close()
				return
			}
		}
	}
}

// flush is used to flush netpoll.Writer.
func (q *ShardQueue) flush() {
	err := q.conn.Writer().Flush()
	if err != nil {
		q.conn.Close()
		return
	}
}

// lock shard.
func (q *ShardQueue) lock(shard int32) {
	for !atomic.CompareAndSwapInt32(&q.locks[shard], 0, 1) {
		runtime.Gosched()
	}
}

// unlock shard.
func (q *ShardQueue) unlock(shard int32) {
	atomic.StoreInt32(&q.locks[shard], 0)
}
