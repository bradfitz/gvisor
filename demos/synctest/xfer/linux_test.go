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
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"gvisor.dev/gvisor/demos/synctest/uml"
	"gvisor.dev/gvisor/demos/synctest/umltest"
)

// TestLinux sends a bulk TCP stream from one Linux kernel to another
// over the simulated link.
//
// In addition to the settings in the package documentation, it reads:
//
//	UML_KERNEL     path of the User-Mode Linux kernel to boot (required)
//	XFER_SYSCTLS   comma-separated sysctls for the sender's kernel, such
//	               as net.ipv4.tcp_sack=0,net.ipv4.tcp_recovery=0
//	XFER_RSYSCTLS  the same, for the receiver's kernel
func TestLinux(t *testing.T) {
	kernel, initrd := umltest.Kernel(t), umltest.Initrd(t)
	p := loadParams(t)

	realStart := umltest.RealNow()
	synctest.Test(t, func(t *testing.T) {
		link := newLink(t, p)

		recvEnv := []string{"GUEST_MODE=tcp-sink", "GUEST_IP=10.0.0.1/24"}
		if s := os.Getenv("XFER_RSYSCTLS"); s != "" {
			recvEnv = append(recvEnv, "GUEST_SYSCTLS="+s)
		}
		sendEnv := []string{"GUEST_MODE=tcp-send", "GUEST_IP=10.0.0.2/24", "GUEST_PEER=10.0.0.1:5001", fmt.Sprintf("GUEST_BYTES=%d", p.bytes)}
		var sysctls []string
		if p.cc != "" {
			sysctls = append(sysctls, "net.ipv4.tcp_congestion_control="+p.cc)
		}
		if s := os.Getenv("XFER_SYSCTLS"); s != "" {
			sysctls = append(sysctls, s)
		}
		if len(sysctls) > 0 {
			sendEnv = append(sendEnv, "GUEST_SYSCTLS="+strings.Join(sysctls, ","))
		}
		receiver := umltest.NewGuest(t, kernel, initrd, "receiver", recvEnv...)
		sender := umltest.NewGuest(t, kernel, initrd, "sender", sendEnv...)

		var recvNIC, sendNIC *uml.NIC
		var err error
		if recvNIC, err = receiver.AddNIC(net.HardwareAddr{2, 0, 0, 0, 0, 1}, link.forward("rcvr", func(f []byte) { sendNIC.Send(f) })); err != nil {
			t.Fatal(err)
		}
		if sendNIC, err = sender.AddNIC(net.HardwareAddr{2, 0, 0, 0, 0, 2}, link.forward("sndr", func(f []byte) { recvNIC.Send(f) })); err != nil {
			t.Fatal(err)
		}
		if err := receiver.Start(); err != nil {
			t.Fatal(err)
		}
		if err := sender.Start(); err != nil {
			t.Fatal(err)
		}

		<-sender.Done()
		<-receiver.Done()
		real := umltest.RealNow() - realStart

		for _, g := range []*umltest.Guest{sender, receiver} {
			for _, l := range g.Lines() {
				if strings.HasPrefix(l, "RESULT ") || strings.HasPrefix(l, "FATAL") {
					t.Logf("%s: %s", g.Name, l)
				}
			}
		}
		// The receiver reports "RESULT received <n> bytes in <duration>".
		var (
			received int64
			elapsed  time.Duration
		)
		for _, l := range receiver.Lines() {
			var d string
			if _, err := fmt.Sscanf(l, "RESULT received %d bytes in %s", &received, &d); err == nil {
				if elapsed, err = time.ParseDuration(d); err != nil {
					t.Fatalf("parsing %q: %v", l, err)
				}
			}
		}
		if received != p.bytes {
			t.Fatalf("receiver reported %d bytes, want %d", received, p.bytes)
		}
		link.report("linux", received, elapsed, real)
		t.Logf("guest wake-ups: sender %+v, receiver %+v", sender.Stats(), receiver.Stats())
	})
}
