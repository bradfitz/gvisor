// Copyright 2024 The gVisor Authors.
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

package tcp

import (
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
)

// deliveryRateSample contains the result of a single delivery rate measurement,
// computed when an ACK arrives.
//
// +stateify savable
type deliveryRateSample struct {
	// deliveryRate is the measured delivery rate in bytes/second.
	deliveryRate int64

	// isAppLimited indicates the sample was taken while the sender was
	// application-limited (i.e. not sending as fast as the network allowed).
	isAppLimited bool

	// rtt is the RTT measured for this sample.
	rtt time.Duration

	// delivered is the number of bytes newly delivered in this ACK.
	delivered int64

	// interval is the time interval over which delivered bytes were measured.
	interval time.Duration
}

// deliveryRateTracker tracks the state needed to compute per-ACK delivery rate
// samples. It follows the algorithm from the BBR draft and Linux tcp_rate.c.
//
// +stateify savable
type deliveryRateTracker struct {
	// delivered is the running total of bytes delivered (ACKed) so far.
	delivered int64

	// deliveredTime is the wall-clock time when delivered was last updated.
	deliveredTime tcpip.MonotonicTime

	// firstSentTime is the send time of the most recently sent data packet.
	firstSentTime tcpip.MonotonicTime

	// appLimited records the value of delivered at the point we became
	// app-limited. If non-zero, we are currently app-limited.
	appLimited int64

	// appLimitedAtPacing is true if the connection is currently
	// application-limited.
	appLimitedAtPacing bool

	// lastSample holds the most recent delivery rate sample from onAck.
	lastSample deliveryRateSample
}

// onSend stamps a segment with the current delivery state. This must only
// stamp the segment on its first transmission (xmitCount == 1), not on
// retransmissions, to avoid corrupting delivery rate measurement.
func (d *deliveryRateTracker) onSend(seg *segment) {
	if seg.xmitCount != 1 {
		return
	}
	seg.deliveredAtSend = d.delivered
	seg.deliveredTimeAtSend = d.deliveredTime
	seg.firstSentTimeAtSend = d.firstSentTime
	seg.isAppLimitedAtSend = d.appLimitedAtPacing

	d.firstSentTime = seg.xmitTime
}

// onAck computes a delivery rate sample from the given ACKed segment.
// now is the current monotonic clock time.
func (d *deliveryRateTracker) onAck(seg *segment, now tcpip.MonotonicTime) deliveryRateSample {
	// Update running delivered count.
	delivered := int64(seg.payloadSize())
	if delivered == 0 {
		delivered = 1 // count pure ACKs as at least 1 byte
	}
	d.delivered += delivered
	d.deliveredTime = now

	// If we have app-limited state and we've delivered past that point,
	// clear it.
	if d.appLimited != 0 && d.delivered > d.appLimited {
		d.appLimited = 0
		d.appLimitedAtPacing = false
	}

	// Compute delivery rate.
	sendElapsed := seg.xmitTime.Sub(seg.firstSentTimeAtSend)
	ackElapsed := now.Sub(seg.deliveredTimeAtSend)

	// Use the larger of send and ack intervals to handle cases where
	// one clock is unreliable.
	interval := sendElapsed
	if ackElapsed > interval {
		interval = ackElapsed
	}

	deliveredBytes := d.delivered - seg.deliveredAtSend

	var rate int64
	if interval > 0 && deliveredBytes > 0 {
		// rate in bytes per second.
		rate = deliveredBytes * int64(time.Second) / int64(interval)
	}

	rtt := now.Sub(seg.xmitTime)

	sample := deliveryRateSample{
		deliveryRate: rate,
		isAppLimited: seg.isAppLimitedAtSend,
		rtt:          rtt,
		delivered:    deliveredBytes,
		interval:     interval,
	}

	// Keep the sample with the highest delivery rate as the best for
	// this ACK event.
	if sample.deliveryRate >= d.lastSample.deliveryRate {
		d.lastSample = sample
	}

	return sample
}

// setAppLimited marks the connection as application-limited at the current
// delivered count plus the given outstanding byte count. This means we won't
// clear the app-limited flag until all currently outstanding data has been
// delivered.
func (d *deliveryRateTracker) setAppLimited(outstandingBytes int64) {
	d.appLimited = d.delivered + outstandingBytes
	d.appLimitedAtPacing = true
}

// isAppLimitedNow returns whether the connection is currently app-limited.
func (d *deliveryRateTracker) isAppLimitedNow() bool {
	return d.appLimitedAtPacing
}
