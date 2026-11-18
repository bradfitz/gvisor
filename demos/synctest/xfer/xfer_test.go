// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package xfer measures bulk TCP transfers over a simulated link with
// latency and packet loss, on the fake clock of a testing/synctest
// bubble.
//
// TestNetstack runs the transfer between two netstack instances, and
// TestLinux runs it between two Linux kernels. Both use the same
// simulated link, so their results are directly comparable.
//
// The tests are tunable with environment variables:
//
//	XFER_BYTES     bytes to send (default 16 MiB)
//	XFER_DELAY     one-way link delay (default 180ms)
//	XFER_LOSS      loss probability per frame, each way (default 0.01)
//	XFER_JITTER    extra random one-way delay per frame, up to this
//	               much (default 0)
//	XFER_CC        congestion control algorithm of the sender, such as
//	               cubic or reno (default: the stack's default)
//	XFER_PKTTRACE  from,to: log every TCP segment put on the link
//	               between these offsets into the test, for example
//	               300s,303s
package xfer

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/demos/synctest/simnet"
)

// params are the settings common to the transfer tests.
type params struct {
	bytes  int64
	delay  time.Duration
	jitter time.Duration
	loss   float64
	cc     string

	traceFrom, traceTo time.Duration
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadParams reads the test settings from the environment.
func loadParams(t *testing.T) params {
	t.Helper()
	var (
		p   params
		err error
	)
	if p.bytes, err = strconv.ParseInt(envOr("XFER_BYTES", strconv.Itoa(16<<20)), 10, 64); err != nil {
		t.Fatalf("XFER_BYTES: %v", err)
	}
	if p.delay, err = time.ParseDuration(envOr("XFER_DELAY", "180ms")); err != nil {
		t.Fatalf("XFER_DELAY: %v", err)
	}
	if p.jitter, err = time.ParseDuration(envOr("XFER_JITTER", "0s")); err != nil {
		t.Fatalf("XFER_JITTER: %v", err)
	}
	if p.loss, err = strconv.ParseFloat(envOr("XFER_LOSS", "0.01"), 64); err != nil {
		t.Fatalf("XFER_LOSS: %v", err)
	}
	p.cc = os.Getenv("XFER_CC")
	if v := os.Getenv("XFER_PKTTRACE"); v != "" {
		from, to, _ := strings.Cut(v, ",")
		if p.traceFrom, err = time.ParseDuration(from); err != nil {
			t.Fatalf("XFER_PKTTRACE: %v", err)
		}
		if p.traceTo, err = time.ParseDuration(to); err != nil {
			t.Fatalf("XFER_PKTTRACE: %v", err)
		}
	}
	return p
}

// testLink is the simulated link between the receiver and the sender
// of a transfer.
type testLink struct {
	*simnet.Link
	t     *testing.T
	p     params
	start time.Time
}

// newLink returns the link for a transfer. It must be called inside
// the synctest bubble.
func newLink(t *testing.T, p params) *testLink {
	l := &testLink{
		Link:  &simnet.Link{Delay: p.delay, Loss: p.loss, Jitter: p.jitter},
		t:     t,
		p:     p,
		start: time.Now(),
	}
	if p.traceTo != 0 {
		l.OnDrop = func(frame []byte) { l.trace("DROP", frame) }
	}
	return l
}

func (l *testLink) trace(who string, frame []byte) {
	if at := time.Since(l.start); at >= l.p.traceFrom && at < l.p.traceTo {
		l.t.Logf("pkt %9.3fs %s %s", at.Seconds(), who, describeTCP(frame))
	}
}

// forward returns a func that sends a frame across the link to
// deliver. The receiver's side of the link must call forward first,
// then the sender's. The name identifies the transmitting end in
// packet traces.
func (l *testLink) forward(name string, deliver func(frame []byte)) func(frame []byte) {
	next := l.Link.Forward(deliver)
	if l.p.traceTo == 0 {
		return next
	}
	return func(frame []byte) {
		l.trace(name, frame)
		next(frame)
	}
}

// report logs the link's statistics and the result of the transfer.
// The elapsed time is the virtual time that the receiver took to
// receive all the data, and real is the real time the test took.
func (l *testLink) report(stack string, received int64, elapsed, real time.Duration) {
	ls := l.Stats()
	l.t.Logf("link: %v one-way delay, up to %v jitter, %.2f%% loss; %d frames sent, %d dropped (%.2f%%); %d from the receiver, %d from the sender",
		l.p.delay, l.p.jitter, l.p.loss*100, ls.Sent, ls.Dropped, 100*float64(ls.Dropped)/float64(ls.Sent), ls.DirSent[0], ls.DirSent[1])
	l.t.Logf("RESULT %s: received %d bytes in %v of virtual time (%.0f kB/s), %v of real time (%.0fx)",
		stack, received, elapsed.Round(time.Millisecond), float64(received)/elapsed.Seconds()/1000,
		real.Round(time.Millisecond), elapsed.Seconds()/real.Seconds())
}

// describeTCP summarizes the TCP segment in an Ethernet frame, with
// sequence numbers in units of 1448-byte segments for readability.
func describeTCP(eth []byte) string {
	if len(eth) < 14+20+20 || eth[12] != 0x08 || eth[13] != 0x00 || eth[23] != 6 {
		return fmt.Sprintf("non-TCP frame of %d bytes", len(eth))
	}
	ip := eth[14:]
	ihl := int(ip[0]&0xf) * 4
	if len(ip) < ihl+20 {
		return fmt.Sprintf("short frame of %d bytes", len(eth))
	}
	tcpHdr := ip[ihl:]
	be32 := func(b []byte) uint32 { return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) }
	seq, ack := be32(tcpHdr[4:]), be32(tcpHdr[8:])
	off := int(tcpHdr[12]>>4) * 4
	if off < 20 || len(tcpHdr) < off {
		return fmt.Sprintf("short frame of %d bytes", len(eth))
	}
	payload := int(ip[2])<<8 | int(ip[3]) - ihl - off
	win := int(tcpHdr[14])<<8 | int(tcpHdr[15])
	var sacks []string
	for opts := tcpHdr[20:off]; len(opts) >= 2; {
		kind, l := opts[0], int(opts[1])
		if kind == 0 {
			break
		}
		if kind == 1 {
			opts = opts[1:]
			continue
		}
		if l < 2 || l > len(opts) {
			break
		}
		if kind == 5 {
			for b := opts[2:l]; len(b) >= 8; b = b[8:] {
				sacks = append(sacks, fmt.Sprintf("%.1f-%.1f", float64(be32(b))/1448, float64(be32(b[4:]))/1448))
			}
		}
		opts = opts[l:]
	}
	return fmt.Sprintf("seq=%.1f ack=%.1f len=%d win=%d sack=%v", float64(seq)/1448, float64(ack)/1448, payload, win, sacks)
}
