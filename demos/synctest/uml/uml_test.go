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

package uml_test

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"gvisor.dev/gvisor/demos/synctest/uml"
	"gvisor.dev/gvisor/demos/synctest/umltest"
)

// TestSleep boots a guest whose init process sleeps for a total of
// 24 hours, and checks that the bubble's clock moved by 24 hours
// while very little real time passed.
func TestSleep(t *testing.T) {
	kernel, initrd := umltest.Kernel(t), umltest.Initrd(t)
	realStart := umltest.RealNow()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		g := umltest.NewGuest(t, kernel, initrd, "sleeper", "GUEST_MODE=sleep", "GUEST_INTERVAL=1h", "GUEST_COUNT=24")
		if err := g.Start(); err != nil {
			t.Fatal(err)
		}

		// Meanwhile, a goroutine on the Go side has timers of its
		// own, interleaved with the guest's.
		goTicks := 0
		stopTicks := make(chan struct{})
		ticksDone := make(chan struct{})
		go func() {
			defer close(ticksDone)
			tick := time.NewTicker(25 * time.Minute)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					goTicks++
				case <-stopTicks:
					return
				}
			}
		}()

		<-g.Done()
		close(stopTicks)
		<-ticksDone
		elapsed := time.Since(start)
		t.Logf("virtual time elapsed: %v; stats: %+v", elapsed, g.Stats())
		if elapsed < 24*time.Hour || elapsed > 24*time.Hour+time.Minute {
			t.Errorf("virtual elapsed = %v; want just over 24h", elapsed)
		}
		if want := int(elapsed / (25 * time.Minute)); goTicks != want {
			t.Errorf("Go-side ticks = %d; want %d", goTicks, want)
		}
		if !g.HasLine("slept 24") {
			t.Errorf("guest did not report its 24th sleep; lines: %q", g.Lines())
		}
		// The guest's wall clock agrees with the bubble's, which
		// starts at midnight UTC 2000-01-01.
		if !strings.Contains(g.ConsoleText(), "guest 2000-01-02T00:00:0") {
			t.Errorf("no guest log line stamped just after 2000-01-02T00:00:00")
		}
	})
	real := umltest.RealNow() - realStart
	t.Logf("real time: %v", real.Round(time.Millisecond))
	if real > time.Minute {
		t.Errorf("took %v of real time", real)
	}
}

// etherFrame returns a broadcast Ethernet frame of the EtherType that
// the "ether" mode of guestinit uses.
func etherFrame(src net.HardwareAddr, msg string) []byte {
	f := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	f = append(f, src...)
	f = append(f, 0x88, 0xB5)
	f = append(f, msg...)
	for len(f) < 60 {
		f = append(f, 0)
	}
	return f
}

// TestEther exchanges raw Ethernet frames with a guest over virtual
// hours. The guest sends a frame every hour on its own timer, and the
// test pings it every 37 minutes on a Go timer. Each side's frames
// carry its own idea of the time, which must agree with the other's.
func TestEther(t *testing.T) {
	kernel, initrd := umltest.Kernel(t), umltest.Initrd(t)
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		g := umltest.NewGuest(t, kernel, initrd, "ether", "GUEST_MODE=ether", "GUEST_INTERVAL=1h", "GUEST_COUNT=5")

		var (
			mu  sync.Mutex
			got []string
		)
		// checkStamp checks a guest wall clock reading against
		// the bubble's clock at the moment the frame arrived.
		checkStamp := func(msg, unixNano string) {
			var ns int64
			fmt.Sscan(unixNano, &ns)
			skew := time.Since(time.Unix(0, ns))
			if skew < -time.Millisecond || skew > time.Millisecond {
				t.Errorf("%q: guest clock is %v behind the bubble clock", msg, skew)
			}
		}
		nic, err := g.AddNIC(net.HardwareAddr{2, 0, 0, 0, 0, 1}, func(frame []byte) {
			if len(frame) < 14 || frame[12] != 0x88 || frame[13] != 0xB5 {
				return
			}
			msg := strings.TrimRight(string(frame[14:]), "\x00")
			if f := strings.Fields(msg); len(f) == 3 {
				checkStamp(msg, f[2])
				msg = f[0] + " " + f[1]
			}
			mu.Lock()
			defer mu.Unlock()
			got = append(got, fmt.Sprintf("%v %s", time.Since(start).Round(time.Second), msg))
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := g.Start(); err != nil {
			t.Fatal(err)
		}

		pingerDone := make(chan struct{})
		go func() {
			defer close(pingerDone)
			tick := time.NewTicker(37 * time.Minute)
			defer tick.Stop()
			for i := 1; ; i++ {
				select {
				case <-tick.C:
					nic.Send(etherFrame(net.HardwareAddr{2, 0, 0, 0, 0, 0xfe}, fmt.Sprintf("ping %d", i)))
				case <-g.Done():
					return
				}
			}
		}()
		<-g.Done()
		<-pingerDone

		want := []string{
			"37m0s pong 1",
			"1h0m0s tick 1",
			"1h14m0s pong 2",
			"1h51m0s pong 3",
			"2h0m0s tick 2",
			"2h28m0s pong 4",
			"3h0m0s tick 3",
			"3h5m0s pong 5",
			"3h42m0s pong 6",
			"4h0m0s tick 4",
			"4h19m0s pong 7",
			"4h56m0s pong 8",
			"5h0m0s tick 5",
		}
		mu.Lock()
		defer mu.Unlock()
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("events:\n got: %q\nwant: %q", got, want)
		}
		tx, rx := nic.Counts()
		t.Logf("virtual time: %v; guest tx=%d rx=%d frames; stats: %+v", time.Since(start), tx, rx, g.Stats())
	})
}

// TestTCPKeepAlive runs two guests connected by a virtual Ethernet
// cable. One opens a TCP connection to the other with SO_KEEPALIVE
// and the kernel's default settings (first probe after two idle
// hours, then every 75 seconds, giving up after nine unanswered
// probes) and sits in a blocking read. After five hours the cable is
// cut. The test observes the keepalive behavior of the Linux TCP
// stack from the wire: every packet is a real packet from a real
// kernel, on virtual time.
func TestTCPKeepAlive(t *testing.T) {
	kernel, initrd := umltest.Kernel(t), umltest.Initrd(t)
	realStart := umltest.RealNow()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		server := umltest.NewGuest(t, kernel, initrd, "server", "GUEST_MODE=tcp-server", "GUEST_IP=10.0.0.1/24")
		client := umltest.NewGuest(t, kernel, initrd, "client", "GUEST_MODE=tcp-client", "GUEST_IP=10.0.0.2/24", "GUEST_PEER=10.0.0.1:80")

		var (
			mu     sync.Mutex
			cut    bool
			frames []string // what the client sent, and when
		)
		var serverNIC, clientNIC *uml.NIC
		var err error
		serverNIC, err = server.AddNIC(net.HardwareAddr{2, 0, 0, 0, 0, 1}, func(frame []byte) {
			mu.Lock()
			defer mu.Unlock()
			if !cut {
				clientNIC.Send(frame)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		clientNIC, err = client.AddNIC(net.HardwareAddr{2, 0, 0, 0, 0, 2}, func(frame []byte) {
			mu.Lock()
			defer mu.Unlock()
			// Note IPv4 TCP segments, which have EtherType 0x0800
			// and IP protocol 6, and ARP, which is EtherType 0x0806.
			at := time.Since(start).Round(time.Second)
			switch {
			case len(frame) > 34 && frame[12] == 0x08 && frame[13] == 0x00 && frame[23] == 6:
				frames = append(frames, fmt.Sprintf("tcp@%v", at))
			case len(frame) > 14 && frame[12] == 0x08 && frame[13] == 0x06:
				frames = append(frames, fmt.Sprintf("arp@%v", at))
			}
			if !cut {
				serverNIC.Send(frame)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := server.Start(); err != nil {
			t.Fatal(err)
		}
		if err := client.Start(); err != nil {
			t.Fatal(err)
		}

		time.Sleep(5 * time.Hour)
		mu.Lock()
		cut = true
		beforeCut := frames
		frames = nil
		mu.Unlock()
		t.Logf("cut the cable at %v; before that the client sent: %v", time.Since(start), beforeCut)

		<-client.Done()
		gaveUp := time.Since(start)
		mu.Lock()
		afterCut := frames
		mu.Unlock()
		t.Logf("client gave up at %v; after the cut it sent: %v", gaveUp.Round(time.Second), afterCut)
		for _, l := range client.Lines() {
			t.Logf("client: %s", l)
		}

		count := func(frames []string, kind string) (n int) {
			for _, f := range frames {
				if strings.HasPrefix(f, kind) {
					n++
				}
			}
			return n
		}
		// Before the cut there are four TCP segments: the SYN and
		// the handshake ACK at about 1s, and keepalive probes
		// after two and four hours. The probes are some minutes
		// late because long timeouts land in a coarse level of
		// the kernel's timer wheel.
		if n := count(beforeCut, "tcp"); n != 4 {
			t.Errorf("client sent %d TCP segments before the cut; want 4", n)
		}
		// After the cut the next probe goes out, but then the
		// neighbor entry for the server goes stale, so the rest of
		// the probes turn into unanswered ARP requests.
		if n := count(afterCut, "tcp"); n < 1 {
			t.Errorf("client sent no keepalive probe after the cut")
		}
		if n := count(afterCut, "arp"); n < 3 {
			t.Errorf("client sent %d ARP requests after the cut; want several", n)
		}
		// Nine probes 75 seconds apart take 11m15s to fail,
		// starting two hours after the last answered probe.
		if gaveUp < 6*time.Hour+11*time.Minute || gaveUp > 6*time.Hour+20*time.Minute {
			t.Errorf("client gave up at %v; want between 6h11m and 6h20m", gaveUp)
		}
		found := false
		for _, l := range client.Lines() {
			found = found || strings.HasPrefix(l, "RESULT keepalive-timeout ")
		}
		if !found {
			t.Errorf("client did not report ETIMEDOUT from its read")
		}
		t.Logf("stats: client %+v, server %+v", client.Stats(), server.Stats())
	})
	t.Logf("real time: %v", (umltest.RealNow() - realStart).Round(time.Millisecond))
}
