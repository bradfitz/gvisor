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

// The guestinit program is the init process (PID 1) of the Linux
// guests that the synctest demos boot. It is an ordinary static Linux program with no knowledge
// that the kernel under it is running on simulated time.
//
// Its behavior is selected by environment variables, which the test
// sets by passing KEY=value arguments on the kernel command line:
//
//	GUEST_IP    address/prefix to assign to eth0, such as 10.0.0.2/24
//	GUEST_MODE  what to do; see the modes map
//	GUEST_SYSCTLS  comma-separated sysctls to set first, such as
//	               net.ipv4.tcp_congestion_control=reno,net.ipv4.tcp_sack=0
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// etherType is the EtherType of the test frames used by the "ether"
// mode. It is one of the values reserved for local experiments.
const etherType = 0x88B5

var modes = map[string]func() error{
	"sleep":      modeSleep,
	"dump":       modeDump,
	"ether":      modeEther,
	"tcp-server": modeTCPServer,
	"tcp-client": modeTCPClient,
	"tcp-sink":   modeTCPSink,
	"tcp-send":   modeTCPSend,
}

func main() {
	err := run()
	if err != nil {
		logf("FATAL: %v", err)
	}
	logf("powering off")
	unix.Sync()
	unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	select {}
}

// logf logs to the console, prefixed with the guest's idea of the
// wall time.
func logf(format string, args ...any) {
	fmt.Printf("guest %s: %s\n", time.Now().UTC().Format("2006-01-02T15:04:05.000000"), fmt.Sprintf(format, args...))
}

func run() error {
	for _, m := range []struct{ src, dst, typ string }{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"},
	} {
		os.MkdirAll(m.dst, 0o755)
		if err := unix.Mount(m.src, m.dst, m.typ, 0, ""); err != nil {
			return fmt.Errorf("mount %s: %w", m.dst, err)
		}
	}
	if err := ifUp("lo", netip.Prefix{}); err != nil {
		return err
	}
	if s := os.Getenv("GUEST_IP"); s != "" {
		pfx, err := netip.ParsePrefix(s)
		if err != nil {
			return err
		}
		if err := ifUp("eth0", pfx); err != nil {
			return err
		}
	}
	if err := applySysctls(os.Getenv("GUEST_SYSCTLS")); err != nil {
		return err
	}
	mode := os.Getenv("GUEST_MODE")
	f, ok := modes[mode]
	if !ok {
		return fmt.Errorf("unknown GUEST_MODE %q", mode)
	}
	logf("booted; mode %q", mode)
	return f()
}

// ifUp brings a network interface up, first assigning it an IPv4
// address if pfx is valid, using the classic ioctls.
func ifUp(name string, pfx netip.Prefix) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if pfx.IsValid() {
		if err := ifr.SetInet4Addr(pfx.Addr().AsSlice()); err != nil {
			return err
		}
		if err := unix.IoctlIfreq(fd, unix.SIOCSIFADDR, ifr); err != nil {
			return fmt.Errorf("SIOCSIFADDR %s: %w", name, err)
		}
		if err := ifr.SetInet4Addr(net.CIDRMask(pfx.Bits(), 32)); err != nil {
			return err
		}
		if err := unix.IoctlIfreq(fd, unix.SIOCSIFNETMASK, ifr); err != nil {
			return fmt.Errorf("SIOCSIFNETMASK %s: %w", name, err)
		}
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("SIOCGIFFLAGS %s: %w", name, err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP | unix.IFF_RUNNING)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("SIOCSIFFLAGS %s: %w", name, err)
	}
	return nil
}

func envDuration(name string, def time.Duration) time.Duration {
	if s := os.Getenv(name); s != "" {
		d, err := time.ParseDuration(s)
		if err == nil {
			return d
		}
		logf("bad %s=%q: %v", name, s, err)
	}
	return def
}

func envInt(name string, def int) int {
	if s := os.Getenv(name); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return def
}

// modeSleep sleeps GUEST_COUNT times for GUEST_INTERVAL each.
func modeSleep() error {
	interval := envDuration("GUEST_INTERVAL", time.Hour)
	for i := range envInt("GUEST_COUNT", 3) {
		time.Sleep(interval)
		logf("slept %d", i+1)
	}
	return nil
}

// modeDump sleeps for GUEST_INTERVAL and then prints the file named
// by GUEST_FILE. It is a debugging aid.
func modeDump() error {
	time.Sleep(envDuration("GUEST_INTERVAL", time.Minute))
	b, err := os.ReadFile(os.Getenv("GUEST_FILE"))
	if err != nil {
		return err
	}
	os.Stdout.Write(b)
	return nil
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// modeEther exchanges raw Ethernet frames with the test. Every
// GUEST_INTERVAL it broadcasts "tick <n> <unixnano>", GUEST_COUNT
// times. It answers every "ping <x>" frame it receives with
// "pong <x> <unixnano>".
func modeEther() error {
	if err := ifUp("eth0", netip.Prefix{}); err != nil {
		return err
	}
	ifi, err := net.InterfaceByName("eth0")
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(etherType)))
	if err != nil {
		return err
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(etherType), Ifindex: ifi.Index}); err != nil {
		return err
	}
	send := func(msg string) {
		frame := make([]byte, 0, 64)
		frame = append(frame, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
		frame = append(frame, ifi.HardwareAddr...)
		frame = append(frame, etherType>>8, etherType&0xff)
		frame = append(frame, msg...)
		if _, err := unix.Write(fd, frame); err != nil {
			logf("send: %v", err)
		}
	}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, err := unix.Read(fd, buf)
			if err != nil {
				logf("read: %v", err)
				return
			}
			if n < 14 {
				continue
			}
			msg := string(buf[14:n])
			if x, ok := strings.CutPrefix(msg, "ping "); ok {
				send(fmt.Sprintf("pong %s %d", strings.TrimRight(x, "\x00"), time.Now().UnixNano()))
			}
		}
	}()
	interval := envDuration("GUEST_INTERVAL", time.Hour)
	for i := range envInt("GUEST_COUNT", 3) {
		time.Sleep(interval)
		send(fmt.Sprintf("tick %d %d", i+1, time.Now().UnixNano()))
	}
	// Leave a moment for a final ping to be answered.
	time.Sleep(time.Second)
	return nil
}

// modeTCPServer accepts TCP connections on port 80 and holds them
// open, reading and discarding whatever arrives.
func modeTCPServer() error {
	// Disable Go's default of enabling 15 second keepalives on
	// accepted connections, so that the only keepalives are the
	// client's.
	lc := net.ListenConfig{KeepAlive: -1}
	ln, err := lc.Listen(context.Background(), "tcp", ":80")
	if err != nil {
		return err
	}
	logf("listening on %v", ln.Addr())
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		logf("accepted connection from %v", c.RemoteAddr())
		go func() {
			n, err := io.Copy(io.Discard, c)
			logf("connection from %v ended after %d bytes: %v", c.RemoteAddr(), n, err)
			c.Close()
		}()
	}
}

// modeTCPClient connects to GUEST_PEER, enables TCP keepalives with
// the kernel's default settings, and then sits idle in a read until
// the kernel reports the connection dead.
func modeTCPClient() error {
	// Give the server a moment to start listening.
	time.Sleep(time.Second)
	d := net.Dialer{KeepAlive: -1} // leave the kernel defaults alone
	c, err := d.Dial("tcp", os.Getenv("GUEST_PEER"))
	if err != nil {
		return err
	}
	defer c.Close()
	rc, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_KEEPALIVE, 1)
	})
	if serr != nil {
		return fmt.Errorf("SO_KEEPALIVE: %w", serr)
	}
	start := time.Now()
	logf("connected to %v; idling", c.RemoteAddr())
	_, err = c.Read(make([]byte, 1))
	logf("read failed after %v: %v", time.Since(start).Round(time.Second), err)
	if errors.Is(err, unix.ETIMEDOUT) {
		logf("RESULT keepalive-timeout %d", int(time.Since(start).Seconds()))
	}
	return nil
}

// applySysctls sets the comma-separated key=value sysctls in list, so
// that tests can configure each guest's kernel differently.
func applySysctls(list string) error {
	for _, kv := range strings.Split(list, ",") {
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("bad sysctl %q", kv)
		}
		path := "/proc/sys/" + strings.ReplaceAll(k, ".", "/")
		if err := os.WriteFile(path, []byte(v), 0o644); err != nil {
			return fmt.Errorf("sysctl %s: %w", kv, err)
		}
	}
	return nil
}

// modeTCPSink accepts one TCP connection on port 5001, reads it to
// EOF, and reports how many bytes arrived and how long that took.
func modeTCPSink() error {
	lc := net.ListenConfig{KeepAlive: -1}
	ln, err := lc.Listen(context.Background(), "tcp", ":5001")
	if err != nil {
		return err
	}
	c, err := ln.Accept()
	if err != nil {
		return err
	}
	start := time.Now()
	n, err := io.Copy(io.Discard, c)
	if err != nil {
		return fmt.Errorf("after %d bytes: %w", n, err)
	}
	logf("RESULT received %d bytes in %v", n, time.Since(start).Round(time.Millisecond))
	c.Close()
	// Powering off stops the kernel dead. Stay up long enough for
	// the connection teardown to complete, even with some loss.
	time.Sleep(30 * time.Second)
	return nil
}

// modeTCPSend connects to GUEST_PEER, writes GUEST_BYTES bytes,
// closes the connection, and reports how long that took along with
// the kernel's statistics for the connection.
func modeTCPSend() error {
	total, err := strconv.ParseInt(os.Getenv("GUEST_BYTES"), 10, 64)
	if err != nil {
		return fmt.Errorf("GUEST_BYTES: %w", err)
	}
	// Give the sink a moment to start listening.
	time.Sleep(time.Second)
	d := net.Dialer{KeepAlive: -1}
	c, err := d.Dial("tcp", os.Getenv("GUEST_PEER"))
	if err != nil {
		return err
	}
	tc := c.(*net.TCPConn)
	start := time.Now()

	// Sample the connection's congestion state as it runs.
	rc, err := tc.SyscallConn()
	if err != nil {
		return err
	}
	var (
		samples  int
		cwndSum  uint64
		caStates [8]int
	)
	traceCwnd := os.Getenv("GUEST_CWND_TRACE") != ""
	sampleDone := make(chan struct{})
	stopSampling := make(chan struct{})
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
			rc.Control(func(fd uintptr) {
				info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
				if err != nil {
					return
				}
				samples++
				cwndSum += uint64(info.Snd_cwnd)
				caStates[info.Ca_state&7]++
				// GUEST_CWND_TRACE=1 logs 40 seconds of samples
				// from five minutes in.
				if traceCwnd && samples > 3000 && samples <= 3400 {
					fmt.Printf("cwndtrace %.1f cwnd=%d ssthresh=%d state=%d retrans=%d\n",
						time.Since(start).Seconds(), info.Snd_cwnd, info.Snd_ssthresh, info.Ca_state, info.Total_retrans)
				}
			})
		}
	}()

	buf := make([]byte, 1<<20)
	for sent := int64(0); sent < total; {
		n := min(int64(len(buf)), total-sent)
		if _, err := tc.Write(buf[:n]); err != nil {
			return fmt.Errorf("after %d bytes: %w", sent, err)
		}
		sent += n
	}
	// The writes return once the data is in the socket buffer. Wait
	// for the peer to acknowledge all of it.
	var info *unix.TCPInfo
	var cc string
	for {
		var serr error
		rc.Control(func(fd uintptr) {
			info, serr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
			cc, _ = unix.GetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION)
		})
		if serr != nil {
			return fmt.Errorf("TCP_INFO: %w", serr)
		}
		if info.Unacked == 0 && info.Notsent_bytes == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	elapsed := time.Since(start)
	close(stopSampling)
	<-sampleDone
	logf("RESULT sent %d bytes in %v", total, elapsed.Round(time.Millisecond))
	logf("RESULT tcpinfo cc=%s retrans=%d rtt=%v rttvar=%v cwnd=%d ssthresh=%d mss=%d",
		cc, info.Total_retrans,
		time.Duration(info.Rtt)*time.Microsecond, time.Duration(info.Rttvar)*time.Microsecond,
		info.Snd_cwnd, info.Snd_ssthresh, info.Snd_mss)
	logf("RESULT tcpinfo2 rto=%v total_rto=%d total_rto_recoveries=%d total_rto_time=%dms dsack_dups=%d reord_seen=%d",
		time.Duration(info.Rto)*time.Microsecond, info.Total_rto, info.Total_rto_recoveries, info.Total_rto_time, info.Dsack_dups, info.Reord_seen)
	if samples > 0 {
		pct := func(n int) float64 { return 100 * float64(n) / float64(samples) }
		// The states are the kernel's TCP_CA_* values.
		logf("RESULT sampled avg_cwnd=%.1f open=%.1f%% disorder=%.1f%% cwr=%.1f%% recovery=%.1f%% loss=%.1f%%",
			float64(cwndSum)/float64(samples), pct(caStates[0]), pct(caStates[1]), pct(caStates[2]), pct(caStates[3]), pct(caStates[4]))
	}
	tc.Close()
	// Stay up long enough for the connection teardown to complete.
	time.Sleep(10 * time.Second)
	return nil
}
