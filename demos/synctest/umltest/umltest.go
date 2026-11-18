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

// Package umltest has helpers for tests that boot uml guests.
package umltest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/demos/synctest/initramfs"
	"gvisor.dev/gvisor/demos/synctest/uml"
	"gvisor.dev/gvisor/pkg/test/testutil"
)

// Kernel returns the path of the User-Mode Linux kernel in the
// UML_KERNEL environment variable. It skips the test if the variable
// is not set.
//
// It does file I/O, so call it before entering a synctest bubble.
func Kernel(t testing.TB) string {
	t.Helper()
	p := os.Getenv("UML_KERNEL")
	if p == "" {
		t.Skip("UML_KERNEL is not set; build a kernel with demos/synctest/kernel/build.sh and set it to the path of the resulting linux binary")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("UML_KERNEL: %v", err)
	}
	return p
}

// Initrd builds an initramfs whose /init is the guestinit program and
// returns its path.
//
// It does file I/O, so call it before entering a synctest bubble.
func Initrd(t testing.TB) string {
	t.Helper()
	bin, err := testutil.FindFile("demos/synctest/guestinit/guestinit")
	if err != nil {
		t.Fatalf("finding guestinit: %v", err)
	}
	init, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	// The directory name must be short: see NewGuest.
	dir, err := os.MkdirTemp("", "initrd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "initramfs.cpio")
	if err := initramfs.WriteFile(path, []initramfs.Entry{
		initramfs.Dir("dev"),
		initramfs.CharDev("dev/console", 5, 1),
		initramfs.Dir("proc"),
		initramfs.Dir("sys"),
		initramfs.Dir("tmp"),
		initramfs.Dir("etc"),
		initramfs.File("init", init),
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

// Guest is a VM that runs guestinit, with its console captured.
type Guest struct {
	*uml.VM

	// Name is the name the guest was created with.
	Name string

	t       testing.TB
	console *os.File
}

// NewGuest returns an unstarted VM that boots the given kernel and
// initrd. The env strings, of the form KEY=value, become environment
// variables of guestinit and select what it does.
//
// The guest is closed, and its console logged if the test failed or
// is verbose, when the test ends. NewGuest must be called inside the
// synctest bubble.
//
// Setting UML_VERBOSE in the environment of the test makes the guest
// kernel log its boot messages. UML_TRACE logs each message of the
// time-travel protocol. UML_SLACK overrides uml.Config.Slack.
func NewGuest(t testing.TB, kernel, initrd, name string, env ...string) *Guest {
	t.Helper()
	console, err := os.CreateTemp("", "umlconsole")
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if os.Getenv("UML_VERBOSE") == "" {
		args = append(args, "quiet")
	}
	cfg := uml.Config{
		Name:    name,
		Kernel:  kernel,
		Initrd:  initrd,
		Args:    append(args, env...),
		Console: console,
		Logf:    t.Logf,
		Trace:   os.Getenv("UML_TRACE") != "",
	}
	if s := os.Getenv("UML_SLACK"); s != "" {
		if cfg.Slack, err = time.ParseDuration(s); err != nil {
			t.Fatal(err)
		}
	}
	vm, err := uml.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	g := &Guest{VM: vm, Name: name, t: t, console: console}
	t.Cleanup(func() {
		vm.Close()
		if t.Failed() || testing.Verbose() {
			t.Logf("console of %s:\n%s", name, g.ConsoleText())
		}
		console.Close()
		os.Remove(console.Name())
	})
	return g
}

// ConsoleText returns everything the guest has written to its console.
func (g *Guest) ConsoleText() string {
	b, err := os.ReadFile(g.console.Name())
	if err != nil {
		g.t.Fatal(err)
	}
	return string(b)
}

// Lines returns the console lines that guestinit logged, without
// their "guest <timestamp>: " prefixes.
func (g *Guest) Lines() (lines []string) {
	for _, l := range strings.Split(g.ConsoleText(), "\n") {
		if rest, ok := strings.CutPrefix(l, "guest "); ok {
			if _, msg, ok := strings.Cut(rest, ": "); ok {
				lines = append(lines, strings.TrimRight(msg, "\r"))
			}
		}
	}
	return lines
}

// HasLine reports whether guestinit logged exactly the given line.
func (g *Guest) HasLine(want string) bool {
	for _, l := range g.Lines() {
		if l == want {
			return true
		}
	}
	return false
}

// RealNow returns the real monotonic time. Inside a synctest bubble
// the time package reports only fake time, so this asks the kernel.
func RealNow() time.Duration {
	var ts unix.Timespec
	unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return time.Duration(ts.Nano())
}
