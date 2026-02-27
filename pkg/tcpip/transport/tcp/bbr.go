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

// BBR mode constants.
const (
	bbrStartup  uint8 = iota // Exponential BW growth to fill pipe.
	bbrDrain                  // Drain queue created during Startup.
	bbrProbeBW                // Cruise, periodically probe for more BW.
	bbrProbeRTT               // Reduce inflight to measure RTprop.
)

// BBR ProbeBW sub-phase constants.
const (
	bbrProbeBWDown   uint8 = iota // Drain excess queue after probing.
	bbrProbeBWCruise              // Steady state, sending at estimated BDP.
	bbrProbeBWRefill              // Refill pipe before probing up.
	bbrProbeBWUp                  // Probe for more bandwidth.
)

// BBR gain constants (scaled by 1000 to avoid floating point).
const (
	// bbrHighGain is the gain used during Startup and ProbeBW Up phase
	// (2/ln(2) ~= 2.885, we use 2885/1000).
	bbrHighGain = 2885

	// bbrDrainGain is the reciprocal of the high gain, used during Drain.
	bbrDrainGain = 346 // 1000/2885 ≈ 346

	// bbrCruiseGain is the steady-state pacing gain.
	bbrCruiseGain = 1000

	// bbrCwndGain is the cwnd gain in Startup (2 to allow for delayed/stretched ACKs).
	bbrStartupCwndGain = 2000

	// bbrProbeBWCwndGain is the cwnd gain during ProbeBW (allow some headroom).
	bbrProbeBWCwndGain = 2000

	// bbrProbeRTTCwnd is the minimum cwnd maintained during ProbeRTT phase.
	bbrProbeRTTCwnd = 4

	// bbrProbeRTTDuration is how long to hold the reduced cwnd in ProbeRTT.
	bbrProbeRTTDuration = 200 * time.Millisecond

	// bbrProbeRTTInterval is how often to enter ProbeRTT to re-measure RTprop.
	bbrProbeRTTInterval = 10 * time.Second

	// bbrBWWindowLen is the length (in round trips) of the BtlBw windowed max filter.
	bbrBWWindowLen = 10

	// bbrFullBWThresh is the threshold for BW growth in Startup. If BW
	// doesn't grow by at least 25% (1250/1000), we count toward Startup exit.
	bbrFullBWThresh = 1250

	// bbrFullBWCount is the number of rounds without sufficient BW growth
	// before we exit Startup.
	bbrFullBWCount = 3

	// bbrStartupLossThresh is the loss rate threshold (2%) for exiting
	// startup in BBRv3. Scaled by 1000 (i.e. 20 = 2%).
	bbrStartupLossThresh = 20

	// bbrBeta is the BBRv3 multiplicative decrease factor (0.7, scaled x1000).
	bbrBeta = 700

	// bbrHeadroom is the BBRv3 headroom factor for inflight_hi (0.85, scaled x1000).
	bbrHeadroom = 850

	// bbrProbeBWUpRounds is the max rounds spent in ProbeBW Up before
	// transitioning back to Down.
	bbrProbeBWUpRounds = 8

	// bbrMinCwnd is the absolute minimum congestion window.
	bbrMinCwnd = 4
)

// bbrState implements the congestionControl interface using the BBRv3 algorithm.
//
// +stateify savable
type bbrState struct {
	s *sender

	// mode is the current BBR operating mode.
	mode uint8

	// probeBWPhase is the current ProbeBW sub-phase.
	probeBWPhase uint8

	// btlBwFilter is a windowed max filter tracking the maximum delivery
	// rate (bottleneck bandwidth estimate) over recent rounds.
	btlBwFilter windowedFilter

	// rtPropFilter is a windowed min filter tracking the minimum RTT
	// (round-trip propagation time) over a 10-second window.
	rtPropFilter windowedFilter

	// rtPropStamp is the wall-clock time when RTprop was last updated.
	rtPropStamp time.Duration

	// probeRTTDoneStamp is the wall-clock time when ProbeRTT can exit
	// (after holding reduced cwnd for bbrProbeRTTDuration).
	probeRTTDoneStamp time.Duration

	// probeRTTRoundDone indicates ProbeRTT has lasted at least one round.
	probeRTTRoundDone bool

	// probeRTTMinStamp is the wall-clock time when we last entered ProbeRTT
	// or otherwise refreshed the RTprop expiry.
	probeRTTMinStamp time.Duration

	// pacingGain is the current pacing gain (scaled x1000).
	pacingGain int

	// cwndGain is the current cwnd gain (scaled x1000).
	cwndGain int

	// Round counting state.

	// delivered is the total delivered count at the start of the current round.
	roundDelivered int64

	// roundCount is the count of packet-timed round trips elapsed.
	roundCount int64

	// roundStart indicates we are at the start of a new round trip.
	roundStart bool

	// Full-pipe detection.

	// fullBw is the max delivery rate observed for full-pipe detection.
	fullBw int64

	// fullBwCount is the number of rounds without 25% BW growth.
	fullBwCount uint8

	// fullBwReached is true once we've determined the pipe is full.
	fullBwReached bool

	// Loss-based bounds (BBRv3).

	// inflightLo is the short-term lower bound on inflight data.
	inflightLo int

	// inflightHi is the max allowed inflight data from ProbeUp.
	inflightHi int

	// bwLo is the short-term lower bound on bandwidth.
	bwLo int64

	// lossRoundDelivered is the delivered count at the start of the
	// current loss round (for loss rate tracking).
	lossRoundDelivered int64

	// lossRoundStart indicates a new loss round has started.
	lossRoundStart bool

	// lossInRound tracks whether any loss was observed in the current round.
	lossInRound bool

	// lossEventsInRound counts loss events in the current round for
	// StartupLoss detection.
	lossEventsInRound int

	// probeBWUpRounds counts rounds spent in ProbeBW Up phase.
	probeBWUpRounds int

	// lastSample is the most recent delivery rate sample.
	lastSample deliveryRateSample

	// priorCwnd saves cwnd before ProbeRTT for restoration.
	priorCwnd int
}

// newBBRCC creates a new BBR congestion control state.
//
// +checklocks:s.ep.mu
func newBBRCC(s *sender) *bbrState {
	b := &bbrState{
		s:          s,
		mode:       bbrStartup,
		pacingGain: bbrHighGain,
		cwndGain:   bbrStartupCwndGain,
		inflightLo: -1, // disabled
		inflightHi: -1, // disabled
		bwLo:       -1, // disabled
	}

	now := wallTime(s)

	b.btlBwFilter.win = time.Duration(bbrBWWindowLen) * time.Second // overridden in round-based terms
	b.btlBwFilter.reset(0, 0)

	b.rtPropFilter.win = bbrProbeRTTInterval
	b.rtPropFilter.reset(int64(effectivelyInfinity), now)

	b.probeRTTMinStamp = now
	b.rtPropStamp = now

	return b
}

// wallTime returns a time.Duration representing the current wall-clock time
// from the endpoint's monotonic clock, suitable for use with the windowed
// filters.
func wallTime(s *sender) time.Duration {
	return s.ep.stack.Clock().NowMonotonic().Sub(tcpipMonotonicZero)
}

// tcpipMonotonicZero is the zero value for tcpip.MonotonicTime, used as a
// reference point for converting MonotonicTime to time.Duration.
var tcpipMonotonicZero tcpip.MonotonicTime

// btlBw returns the current bottleneck bandwidth estimate.
func (b *bbrState) btlBw() int64 {
	bw := b.btlBwFilter.getBest()
	if b.bwLo >= 0 && b.bwLo < bw {
		bw = b.bwLo
	}
	return bw
}

// rtProp returns the current min RTT estimate.
func (b *bbrState) rtProp() time.Duration {
	return time.Duration(b.rtPropFilter.getBest())
}

// bdp computes the estimated bandwidth-delay product in bytes.
func (b *bbrState) bdp() int64 {
	rtp := b.rtProp()
	if rtp <= 0 {
		return int64(b.s.SndCwnd) * int64(b.s.MaxPayloadSize)
	}
	return b.btlBw() * int64(rtp) / int64(time.Second)
}

// inflightTarget returns the target inflight in bytes based on current gains.
func (b *bbrState) inflightTarget() int64 {
	bdp := b.bdp()
	target := bdp * int64(b.cwndGain) / 1000

	// Apply loss-based bounds if set.
	if b.inflightLo >= 0 {
		lo := int64(b.inflightLo) * int64(b.s.MaxPayloadSize)
		if target > lo {
			target = lo
		}
	}
	if b.inflightHi >= 0 {
		hi := int64(b.inflightHi) * int64(b.s.MaxPayloadSize)
		if target > hi {
			target = hi
		}
	}
	return target
}

// setCwnd sets the congestion window based on the current BBR state.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) setCwnd() {
	mss := int64(b.s.MaxPayloadSize)
	if mss == 0 {
		return
	}
	target := b.inflightTarget()

	// Convert to packets (ceiling division).
	cwnd := int((target + mss - 1) / mss)

	// During ProbeRTT, use the minimum cwnd.
	if b.mode == bbrProbeRTT {
		cwnd = bbrProbeRTTCwnd
	}

	// Enforce minimum.
	if cwnd < bbrMinCwnd {
		cwnd = bbrMinCwnd
	}

	b.s.SndCwnd = cwnd
}

// updateModel updates BtlBw and RTprop based on the latest delivery sample.
func (b *bbrState) updateModel() {
	rs := b.lastSample

	// Update round trip counter.
	b.updateRound(rs)

	// Update BtlBw: windowed max of delivery rate.
	// Only take samples that aren't app-limited, or that exceed the current best.
	if rs.deliveryRate > 0 && (!rs.isAppLimited || rs.deliveryRate > b.btlBwFilter.getBest()) {
		b.btlBwFilter.updateMax(time.Duration(b.roundCount)*time.Second, rs.deliveryRate)
	}

	// Update RTprop: windowed min of RTT.
	now := wallTime(b.s)
	if rs.rtt > 0 && rs.rtt < time.Duration(b.rtPropFilter.getBest()) {
		b.rtPropFilter.updateMin(now, int64(rs.rtt))
		b.probeRTTMinStamp = now
	}

	// Update loss tracking.
	b.updateLossDetection()
}

// updateRound maintains round trip counting based on delivered bytes.
func (b *bbrState) updateRound(rs deliveryRateSample) {
	if rs.delivered > 0 && b.s.drt.delivered > b.roundDelivered {
		b.roundStart = true
		b.roundCount++
		b.roundDelivered = b.s.drt.delivered

		// Loss round tracking.
		if b.s.drt.delivered > b.lossRoundDelivered {
			b.lossRoundStart = true
			b.lossRoundDelivered = b.s.drt.delivered
			b.lossInRound = false
			b.lossEventsInRound = 0
		}
	} else {
		b.roundStart = false
		b.lossRoundStart = false
	}
}

// updateLossDetection handles loss-based bounding (BBRv3).
func (b *bbrState) updateLossDetection() {
	// Nothing to do without loss info.
}

// updateControlParameters updates the mode-specific state and transitions.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) updateControlParameters() {
	switch b.mode {
	case bbrStartup:
		b.updateStartup()
	case bbrDrain:
		b.updateDrain()
	case bbrProbeBW:
		b.updateProbeBW()
	case bbrProbeRTT:
		b.updateProbeRTT()
	}

	b.checkProbeRTT()
	b.setCwnd()
}

// updateStartup handles the Startup phase.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) updateStartup() {
	b.checkStartupFullBW()
	b.checkStartupLoss()
}

// checkStartupFullBW checks if BW has stopped growing (pipe is full).
func (b *bbrState) checkStartupFullBW() {
	if b.fullBwReached || !b.roundStart {
		return
	}

	bw := b.btlBwFilter.getBest()
	// Check if BW grew by at least 25%.
	if bw*1000 >= b.fullBw*bbrFullBWThresh {
		b.fullBw = bw
		b.fullBwCount = 0
		return
	}

	b.fullBwCount++
	if b.fullBwCount >= bbrFullBWCount {
		b.fullBwReached = true
		b.enterDrain()
	}
}

// checkStartupLoss exits Startup on sustained loss (BBRv3).
//
// +checklocks:b.s.ep.mu
func (b *bbrState) checkStartupLoss() {
	if b.fullBwReached {
		return
	}
	// If we detect any loss events and have enough rounds, consider exiting.
	if b.lossEventsInRound > 0 && b.roundCount > 2 {
		// Check if loss rate exceeds threshold.
		outstanding := b.s.Outstanding
		if outstanding > 0 && b.lossEventsInRound*1000/outstanding >= bbrStartupLossThresh {
			b.fullBwReached = true
			b.enterDrain()
		}
	}
}

// enterDrain transitions from Startup to Drain.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) enterDrain() {
	b.mode = bbrDrain
	b.pacingGain = bbrDrainGain
	b.cwndGain = bbrStartupCwndGain // maintain startup cwnd gain during drain
}

// updateDrain handles the Drain phase.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) updateDrain() {
	// Exit Drain when inflight drops to BDP.
	inflightBytes := int64(b.s.Outstanding) * int64(b.s.MaxPayloadSize)
	if inflightBytes <= b.bdp() {
		b.enterProbeBW()
	}
}

// enterProbeBW transitions to ProbeBW Cruise.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) enterProbeBW() {
	b.mode = bbrProbeBW
	b.probeBWPhase = bbrProbeBWCruise
	b.pacingGain = bbrCruiseGain
	b.cwndGain = bbrProbeBWCwndGain
}

// updateProbeBW handles the ProbeBW phase cycling.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) updateProbeBW() {
	switch b.probeBWPhase {
	case bbrProbeBWDown:
		b.updateProbeBWDown()
	case bbrProbeBWCruise:
		b.updateProbeBWCruise()
	case bbrProbeBWRefill:
		b.updateProbeBWRefill()
	case bbrProbeBWUp:
		b.updateProbeBWUp()
	}
}

// updateProbeBWDown checks for transition out of Down phase.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) updateProbeBWDown() {
	// Once inflight is at or below BDP, transition to Cruise.
	inflightBytes := int64(b.s.Outstanding) * int64(b.s.MaxPayloadSize)
	if inflightBytes <= b.bdp() {
		b.probeBWPhase = bbrProbeBWCruise
		b.pacingGain = bbrCruiseGain
	}
}

// updateProbeBWCruise handles the Cruise phase.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) updateProbeBWCruise() {
	// Periodically start probing for more bandwidth.
	// Transition to Refill when a new round starts and enough time has passed.
	if b.roundStart {
		b.probeBWPhase = bbrProbeBWRefill
		b.pacingGain = bbrCruiseGain
		// Reset inflight bounds for the new probe cycle.
		b.inflightLo = -1
		b.bwLo = -1
	}
}

// updateProbeBWRefill handles the Refill phase.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) updateProbeBWRefill() {
	// Refill: one round to fill the pipe before probing up.
	if b.roundStart {
		b.probeBWPhase = bbrProbeBWUp
		b.pacingGain = bbrHighGain
		b.probeBWUpRounds = 0
		// Set inflightHi to current inflight as starting point.
		b.inflightHi = b.s.Outstanding
	}
}

// updateProbeBWUp handles the Up phase.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) updateProbeBWUp() {
	if b.roundStart {
		b.probeBWUpRounds++
	}
	// Update inflightHi if we've exceeded it.
	if b.s.Outstanding > b.inflightHi {
		b.inflightHi = b.s.Outstanding
	}
	// Probe for a limited number of rounds then transition to Down.
	if b.probeBWUpRounds >= bbrProbeBWUpRounds || (b.lossInRound && b.roundStart) {
		b.enterProbeBWDown()
	}
}

// enterProbeBWDown transitions to ProbeBW Down phase.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) enterProbeBWDown() {
	b.probeBWPhase = bbrProbeBWDown
	b.pacingGain = bbrDrainGain
}

// checkProbeRTT checks if we need to enter ProbeRTT to re-measure RTprop.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) checkProbeRTT() {
	if b.mode == bbrProbeRTT || b.mode == bbrStartup {
		return
	}
	now := wallTime(b.s)
	if now-b.probeRTTMinStamp > bbrProbeRTTInterval {
		b.enterProbeRTT()
	}
}

// enterProbeRTT transitions to ProbeRTT mode.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) enterProbeRTT() {
	b.priorCwnd = b.s.SndCwnd
	b.mode = bbrProbeRTT
	b.pacingGain = bbrCruiseGain
	b.cwndGain = bbrCruiseGain
	b.probeRTTDoneStamp = 0
	b.probeRTTRoundDone = false
}

// updateProbeRTT handles the ProbeRTT phase.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) updateProbeRTT() {
	now := wallTime(b.s)

	// Wait until inflight drops to probeRTT cwnd.
	if b.probeRTTDoneStamp == 0 {
		if b.s.Outstanding <= bbrProbeRTTCwnd {
			b.probeRTTDoneStamp = now + time.Duration(bbrProbeRTTDuration)
			b.probeRTTRoundDone = false
		}
		return
	}

	if b.roundStart {
		b.probeRTTRoundDone = true
	}

	// Exit ProbeRTT after at least one round and the duration has passed.
	if b.probeRTTRoundDone && now >= b.probeRTTDoneStamp {
		b.probeRTTMinStamp = now
		b.exitProbeRTT()
	}
}

// exitProbeRTT restores state after ProbeRTT.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) exitProbeRTT() {
	// Restore cwnd.
	if b.priorCwnd > b.s.SndCwnd {
		b.s.SndCwnd = b.priorCwnd
	}

	if b.fullBwReached {
		b.enterProbeBW()
	} else {
		b.mode = bbrStartup
		b.pacingGain = bbrHighGain
		b.cwndGain = bbrStartupCwndGain
	}
}

// updateOnLoss handles BBRv3 loss-based inflight bounding.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) updateOnLoss() {
	b.lossInRound = true
	b.lossEventsInRound++

	// Apply loss-based bounds: reduce inflightLo and bwLo.
	if b.mode == bbrProbeBW {
		if b.inflightLo < 0 {
			b.inflightLo = b.s.Outstanding
		} else {
			// Multiplicative decrease.
			b.inflightLo = b.inflightLo * bbrBeta / 1000
		}
		if b.bwLo < 0 {
			b.bwLo = b.btlBwFilter.getBest()
		} else {
			b.bwLo = b.bwLo * bbrBeta / 1000
		}
	}

	// Apply headroom to inflightHi during ProbeBW.
	if b.mode == bbrProbeBW && b.probeBWPhase == bbrProbeBWUp {
		b.inflightHi = max(b.s.Outstanding, b.inflightHi*bbrHeadroom/1000)
	}
}

// --- congestionControl interface implementation ---

// Update implements congestionControl.Update.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) Update(packetsAcked int, rtt time.Duration) {
	// The delivery rate tracker has already been updated per-segment in the
	// ACK processing loop (snd.go). Read the best sample it accumulated.
	b.lastSample = b.s.drt.lastSample
	// Reset the tracker's per-ACK sample accumulator for the next ACK.
	b.s.drt.lastSample = deliveryRateSample{}

	// If we didn't get a delivery rate sample but have an RTT from the
	// sender, use it as a fallback for model updates.
	if b.lastSample.rtt <= 0 && rtt > 0 {
		b.lastSample.rtt = rtt
	}

	b.updateModel()
	b.updateControlParameters()
}

// HandleLossDetected implements congestionControl.HandleLossDetected.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) HandleLossDetected() {
	b.updateOnLoss()
}

// HandleRTOExpired implements congestionControl.HandleRTOExpired.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) HandleRTOExpired() {
	// On RTO, save and restore cwnd; reduce to 1 for retransmission.
	b.priorCwnd = b.s.SndCwnd
	b.s.SndCwnd = 1
}

// PostRecovery implements congestionControl.PostRecovery.
//
// +checklocks:b.s.ep.mu
func (b *bbrState) PostRecovery() {
	// Restore cwnd after recovery.
	if b.priorCwnd > b.s.SndCwnd {
		b.s.SndCwnd = b.priorCwnd
	}
	b.setCwnd()
}

// CustomRecovery implements congestionControl.CustomRecovery.
func (b *bbrState) CustomRecovery() bool {
	return true
}
