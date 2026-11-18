// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package simnet has simulated network links for tests that run in a
// testing/synctest bubble.
package simnet

import (
	"math/rand/v2"
	"sync"
	"time"
)

// Link is a simulated point-to-point Ethernet link with a fixed
// one-way delay and independent random loss in each direction. It
// has unlimited bandwidth and never reorders frames.
//
// Its loss pattern is seeded per direction, so a deterministic sender
// sees the same losses on every run.
type Link struct {
	Delay time.Duration // one-way latency
	Loss  float64       // probability that a frame is dropped

	// Jitter is extra one-way latency, chosen uniformly at random
	// between zero and Jitter for each frame. A frame is never
	// delivered before one sent ahead of it, so a frame that draws
	// a small delay may be held up behind one that drew a large one.
	Jitter time.Duration

	// OnDrop, if non-nil, is called with each frame the link drops.
	// It is for tracing. It must be set before Forward is called.
	OnDrop func(frame []byte)

	mu      sync.Mutex
	dirs    int   // number of Forward calls so far
	dirSent []int // frames offered, by direction
	sent    int
	dropped int
	bytes   int64
}

// Stats describe the traffic a Link has carried, in both directions.
type Stats struct {
	Sent    int   // frames offered to the link
	Dropped int   // frames the link dropped
	Bytes   int64 // bytes of frames delivered

	// DirSent is the number of frames offered in each direction,
	// in the order that Forward was called.
	DirSent []int
}

// Stats returns the link's counters.
func (l *Link) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Stats{Sent: l.sent, Dropped: l.dropped, Bytes: l.bytes, DirSent: append([]int(nil), l.dirSent...)}
}

// Forward returns a func that sends a frame across the link, calling
// deliver with it after the link's delay unless the link drops it.
// The returned func copies the frame and does not block. Call Forward
// once for each direction.
func (l *Link) Forward(deliver func(frame []byte)) func(frame []byte) {
	// Each direction has its own random source, so that which
	// frames are lost in one direction does not depend on how the
	// two directions' frames happen to interleave.
	l.mu.Lock()
	dir := l.dirs
	l.dirs++
	l.dirSent = append(l.dirSent, 0)
	rng := rand.New(rand.NewPCG(1, uint64(dir+1)))
	l.mu.Unlock()

	// Frames in flight, oldest first. Every frame gets its own
	// timer, but timers with the same deadline fire in no particular
	// order and on separate goroutines. So each timer delivers the
	// oldest frame in the queue rather than its own, and holds
	// deliverMu while it does, which keeps the frames in the order
	// they were sent.
	var (
		queueMu   sync.Mutex
		queue     [][]byte
		lastDue   time.Time // when the newest frame in queue is due
		deliverMu sync.Mutex
	)
	deliverNext := func() {
		deliverMu.Lock()
		defer deliverMu.Unlock()
		queueMu.Lock()
		f := queue[0]
		queue = queue[1:]
		queueMu.Unlock()
		deliver(f)
	}
	return func(frame []byte) {
		l.mu.Lock()
		l.sent++
		l.dirSent[dir]++
		drop := rng.Float64() < l.Loss
		delay := l.Delay
		if l.Jitter > 0 {
			delay += time.Duration(rng.Int64N(int64(l.Jitter) + 1))
		}
		if drop {
			l.dropped++
		} else {
			l.bytes += int64(len(frame))
		}
		l.mu.Unlock()
		if drop {
			if l.OnDrop != nil {
				l.OnDrop(frame)
			}
			return
		}
		f := append([]byte(nil), frame...)
		queueMu.Lock()
		due := time.Now().Add(delay)
		if due.Before(lastDue) {
			due = lastDue
		}
		lastDue = due
		queue = append(queue, f)
		queueMu.Unlock()
		time.AfterFunc(time.Until(due), deliverNext)
	}
}
