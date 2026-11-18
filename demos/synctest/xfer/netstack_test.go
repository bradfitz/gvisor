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

package xfer

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"gvisor.dev/gvisor/demos/synctest/umltest"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	nicID = 1

	// linkMTU is the MTU to give the channel endpoint so that, once
	// the Ethernet layer on top of it has taken its share, TCP
	// segments carry 1448 bytes of payload, the same as Linux on a
	// 1500 byte Ethernet.
	linkMTU = 1542
)

// tickingClock is a clock whose monotonic time is strictly
// increasing: no two readings are equal.
//
// In a synctest bubble the clock stands still while goroutines run,
// so everything a stack does in response to one event happens at a
// single instant, as if on an infinitely fast CPU. That gives every
// segment sent in a burst the same transmit time, which no real
// machine does, and which misleads logic that orders segments by
// when they were sent. It also makes a timer that is due at the
// instant an ACK arrives race with that ACK. So the clock charges
// one nanosecond for each reading.
type tickingClock struct {
	tcpip.Clock
	mu   sync.Mutex
	last tcpip.MonotonicTime
}

// NowMonotonic implements tcpip.Clock.NowMonotonic.
func (c *tickingClock) NowMonotonic() tcpip.MonotonicTime {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.Clock.NowMonotonic()
	if !c.last.Before(now) {
		now = c.last.Add(time.Nanosecond)
	}
	c.last = now
	return now
}

// host is a netstack with one Ethernet interface.
type host struct {
	stack  *stack.Stack
	ep     *channel.Endpoint
	addr   tcpip.Address
	cancel context.CancelFunc
	done   chan struct{}
}

// newHost returns a netstack with the given address on a /24. Frames
// that it transmits are passed to send.
//
// XFER_CC selects the congestion control algorithm. XFER_RACK=0
// disables RACK loss detection and XFER_RACK=1 enables it; by default
// the stack's default applies.
func newHost(t *testing.T, mac net.HardwareAddr, ip [4]byte, p params, send func(frame []byte)) *host {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
		Clock:              &tickingClock{Clock: tcpip.NewStdClock()},
	})
	if p.cc != "" {
		cc := tcpip.CongestionControlOption(p.cc)
		if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &cc); err != nil {
			t.Fatalf("setting congestion control %q: %v", p.cc, err)
		}
	}
	if v := os.Getenv("XFER_RACK"); v != "" {
		recovery := tcpip.TCPRecovery(0)
		if v == "1" {
			recovery = tcpip.TCPRACKLossDetection
		}
		if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &recovery); err != nil {
			t.Fatalf("setting recovery: %v", err)
		}
	}

	h := &host{stack: s, addr: tcpip.AddrFrom4(ip), done: make(chan struct{})}
	h.ep = channel.New(4096, linkMTU, tcpip.LinkAddress(mac))
	if err := s.CreateNIC(nicID, ethernet.New(h.ep)); err != nil {
		t.Fatalf("CreateNIC: %v", err)
	}
	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: h.addr, PrefixLen: 24},
	}, stack.AddressProperties{}); err != nil {
		t.Fatalf("AddProtocolAddress: %v", err)
	}
	subnet, err := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{ip[0], ip[1], ip[2], 0}), tcpip.MaskFromBytes([]byte{255, 255, 255, 0}))
	if err != nil {
		t.Fatal(err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: subnet, NIC: nicID}})

	// Pump the frames that the stack transmits out to the link.
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		defer close(h.done)
		for {
			pkt := h.ep.ReadContext(ctx)
			if pkt == nil {
				if ctx.Err() != nil {
					return
				}
				continue
			}
			v := pkt.ToView()
			send(v.AsSlice())
			v.Release()
			pkt.DecRef()
		}
	}()
	return h
}

// deliver hands a received Ethernet frame to the host's stack.
func (h *host) deliver(frame []byte) {
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame)})
	h.ep.InjectInbound(0, pkt)
	pkt.DecRef()
}

func (h *host) close() {
	h.cancel()
	<-h.done
	h.stack.Close()
	h.stack.Wait()
}

// TestNetstack sends a bulk TCP stream from one netstack to another
// over the simulated link.
//
// In addition to the settings in the package documentation, it reads
// XFER_RACK; see newHost.
func TestNetstack(t *testing.T) {
	p := loadParams(t)

	realStart := umltest.RealNow()
	synctest.Test(t, func(t *testing.T) {
		link := newLink(t, p)
		var receiver, sender *host
		receiver = newHost(t, net.HardwareAddr{2, 0, 0, 0, 0, 1}, [4]byte{10, 0, 0, 1}, p,
			link.forward("rcvr", func(f []byte) { sender.deliver(f) }))
		defer receiver.close()
		sender = newHost(t, net.HardwareAddr{2, 0, 0, 0, 0, 2}, [4]byte{10, 0, 0, 2}, p,
			link.forward("sndr", func(f []byte) { receiver.deliver(f) }))
		defer sender.close()

		ln, err := gonet.ListenTCP(receiver.stack, tcpip.FullAddress{NIC: nicID, Addr: receiver.addr, Port: 5001}, ipv4.ProtocolNumber)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()

		type result struct {
			n       int64
			elapsed time.Duration
			err     error
		}
		received := make(chan result, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				received <- result{err: err}
				return
			}
			defer c.Close()
			start := time.Now()
			n, err := io.Copy(io.Discard, c)
			received <- result{n, time.Since(start), err}
		}()

		// Dial by hand rather than with gonet.DialTCP, to keep hold
		// of the endpoint for sampling its congestion state.
		var wq waiter.Queue
		ep, terr := sender.stack.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
		if terr != nil {
			t.Fatal(terr)
		}
		we, notifyCh := waiter.NewChannelEntry(waiter.WritableEvents)
		wq.EventRegister(&we)
		terr = ep.Connect(tcpip.FullAddress{NIC: nicID, Addr: receiver.addr, Port: 5001})
		if _, ok := terr.(*tcpip.ErrConnectStarted); ok {
			<-notifyCh
			terr = ep.LastError()
		}
		wq.EventUnregister(&we)
		if terr != nil {
			t.Fatalf("connect: %v", terr)
		}
		c := gonet.NewTCPConn(&wq, ep)

		// Sample the sender's congestion state as the transfer runs.
		var (
			samples  int
			cwndSum  uint64
			ccStates [8]int
		)
		stopSampling := make(chan struct{})
		sampleDone := make(chan struct{})
		go func() {
			defer close(sampleDone)
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-stopSampling:
					return
				case <-tick.C:
				}
				var info tcpip.TCPInfoOption
				if err := ep.GetSockOpt(&info); err != nil {
					continue
				}
				samples++
				cwndSum += uint64(info.SndCwnd)
				ccStates[int(info.CcState)&7]++
			}
		}()

		buf := make([]byte, 1<<20)
		for sent := int64(0); sent < p.bytes; {
			n := min(int64(len(buf)), p.bytes-sent)
			if _, err := c.Write(buf[:n]); err != nil {
				t.Fatalf("write after %d bytes: %v", sent, err)
			}
			sent += n
		}
		if err := c.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		res := <-received
		real := umltest.RealNow() - realStart
		close(stopSampling)
		<-sampleDone
		if res.err != nil {
			t.Fatalf("receiver: %v after %d bytes", res.err, res.n)
		}
		if res.n != p.bytes {
			t.Errorf("received %d bytes, want %d", res.n, p.bytes)
		}
		var info tcpip.TCPInfoOption
		if err := ep.GetSockOpt(&info); err != nil {
			t.Fatalf("GetSockOpt: %v", err)
		}
		c.Close()

		st := sender.stack.Stats().TCP
		t.Logf("sender: retransmits=%d fast_retransmits=%d sack_recoveries=%d timeouts=%d slow_start_retransmits=%d",
			st.Retransmits.Value(), st.FastRetransmit.Value(), st.SACKRecovery.Value(),
			st.Timeouts.Value(), st.SlowStartRetransmits.Value())
		t.Logf("sender: rto=%v rtt=%v rttvar=%v reorder_seen=%v", info.RTO.Round(time.Millisecond),
			info.RTT.Round(time.Millisecond), info.RTTVar.Round(time.Millisecond), info.ReorderSeen)
		if samples > 0 {
			pct := func(st tcpip.CongestionControlState) float64 { return 100 * float64(ccStates[st]) / float64(samples) }
			t.Logf("sender, sampled: avg_cwnd=%.1f open=%.1f%% disorder=%.1f%% fast_recovery=%.1f%% sack_recovery=%.1f%% rto_recovery=%.1f%%",
				float64(cwndSum)/float64(samples), pct(tcpip.Open), pct(tcpip.Disorder), pct(tcpip.FastRecovery), pct(tcpip.SACKRecovery), pct(tcpip.RTORecovery))
		}
		link.report("netstack", res.n, res.elapsed, real)
		// Let the connection teardown finish before the stacks close.
		time.Sleep(10 * time.Second)
	})
}
