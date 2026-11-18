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

package uml

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Config configures a VM.
type Config struct {
	// Name identifies the VM in logs. It is optional.
	Name string

	// Kernel is the path to the UML kernel executable ("linux").
	// It must be built with CONFIG_UML_TIME_TRAVEL_SUPPORT and
	// CONFIG_VIRTIO_UML.
	Kernel string

	// Initrd is the path to the initramfs to boot.
	Initrd string

	// MemMB is the guest's memory size in megabytes.
	// Zero means 256.
	MemMB int

	// Args are extra kernel command line arguments. Arguments of the
	// form KEY=value that the kernel does not recognize end up in
	// the environment of the guest's init process.
	Args []string

	// Console is where the guest's console output goes.
	// If nil, os.Stderr is used.
	Console *os.File

	// Slack is how far the guest's clock is allowed to run ahead of
	// the bubble's clock while the guest is executing.
	//
	// The guest kernel has infinite CPU speed, but it nudges its
	// clock forward by 256ns every time something reads it, so
	// that code which polls the clock makes progress. With zero
	// Slack every such nudge is a round trip to this package and
	// an idle-detection cycle of the synctest bubble, which is
	// exact but slow. With a non-zero Slack the guest may run
	// ahead on its own by up to Slack before it has to ask. The
	// cost is that an event arriving from the Go side may be
	// delivered up to Slack late in virtual time.
	//
	// Zero means DefaultSlack. Use a negative value for strict mode.
	Slack time.Duration

	// NoPin disables CPU pinning. By default the guest's processes
	// and the goroutine managing them are all pinned to one host
	// CPU. A simulation is a strictly serial game of ping-pong
	// between those processes, so sharing a CPU avoids waking
	// another CPU up for every hand-off, which is several times
	// faster. For the best speed, also run the whole test process
	// on a single CPU, for example with "taskset -c 2 go test".
	NoPin bool

	// Logf, if non-nil, receives debug logs.
	Logf func(format string, args ...any)

	// Trace makes the VM log every time-travel protocol message
	// to Logf.
	Trace bool
}

// DefaultSlack is the default value of Config.Slack.
const DefaultSlack = 100 * time.Microsecond

// ttInfinity is the threshold above which a requested time means
// "never". The kernel uses S64_MAX for an idle system with no timers.
const ttInfinity = 1 << 62

// Stats are counters describing a VM's activity.
type Stats struct {
	Runs      int // number of times the guest was told to run
	TimerRuns int // subset of Runs that were for a guest timer expiring
	IRQRuns   int // subset of Runs that were for a device interrupt
	TTMsgs    int // time-travel protocol messages received
}

// VM is a User-Mode Linux instance whose clock is driven by the
// calling goroutine's synctest bubble.
//
// A VM must be created, started and closed inside the bubble.
type VM struct {
	cfg   Config
	slack uint64 // in nanoseconds; 0 means strict
	dir   string // temp dir holding sockets

	cmd   *exec.Cmd
	pidfd int
	cpu   *unix.CPUSet // CPU to pin everything to, or nil

	ttListen int // listening time-travel socket
	tt       int // connected time-travel socket, or -1

	t0 time.Time // bubble time at which the guest's clock read zero

	// The fields below are owned by the manager goroutine.
	req      uint64 // latest time the guest requested to run at
	reqValid bool
	minTime  uint64 // upper bound on the guest's current local clock

	// Activity counters; see Stats.
	runs, timerRuns, irqRuns, ttMsgs atomic.Int64

	nics []*NIC

	wake     chan struct{} // signaled when a device has input for the guest
	stop     chan struct{} // closed by Close
	done     chan struct{} // closed when the manager goroutine exits
	stopOnce sync.Once

	mu      sync.Mutex
	err     error // why the manager exited
	started bool
}

// New returns a new, unstarted VM.
func New(cfg Config) (*VM, error) {
	if cfg.Kernel == "" {
		return nil, errors.New("uml: Config.Kernel is required")
	}
	if cfg.MemMB == 0 {
		cfg.MemMB = 256
	}
	if cfg.Console == nil {
		cfg.Console = os.Stderr
	}
	vm := &VM{
		cfg:   cfg,
		tt:    -1,
		pidfd: -1,
		wake:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	switch {
	case cfg.Slack == 0:
		vm.slack = uint64(DefaultSlack)
	case cfg.Slack > 0:
		vm.slack = uint64(cfg.Slack)
	}
	if !cfg.NoPin {
		vm.cpu = pinCPU()
	}
	// Unix socket paths are limited to about 108 bytes, so use a
	// short directory name rather than t.TempDir.
	dir, err := os.MkdirTemp("", "uml")
	if err != nil {
		return nil, err
	}
	vm.dir = dir
	vm.ttListen, err = listenUnix(filepath.Join(dir, "tt.sock"))
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return vm, nil
}

func (vm *VM) logf(format string, args ...any) {
	if vm.cfg.Logf != nil {
		vm.cfg.Logf("uml[%s]: "+format, append([]any{vm.cfg.Name}, args...)...)
	}
}

// listenUnix returns a blocking, listening unix stream socket.
func listenUnix(path string) (int, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("bind %s: %w", path, err)
	}
	if err := unix.Listen(fd, 1); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// Start boots the guest and starts the goroutine that manages it.
func (vm *VM) Start() error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.started {
		return errors.New("uml: already started")
	}
	vm.started = true

	args := []string{
		fmt.Sprintf("mem=%dM", vm.cfg.MemMB),
		"time-travel=ext:" + filepath.Join(vm.dir, "tt.sock"),
		"con0=null,fd:1", "con=null", "ssl=null",
		"rdinit=/init",
		// Run guest userspace under seccomp rather than ptrace
		// where the host supports it. It is markedly faster.
		"seccomp=auto",
	}
	if vm.cfg.Initrd != "" {
		args = append(args, "initrd="+vm.cfg.Initrd)
	}
	for _, n := range vm.nics {
		args = append(args, fmt.Sprintf("virtio_uml.device=%s:%d", n.dev.sockPath, virtioIDNet))
	}
	args = append(args, vm.cfg.Args...)

	cmd := exec.Command(vm.cfg.Kernel, args...)
	cmd.Stdout = vm.cfg.Console
	cmd.Stderr = vm.cfg.Console
	cmd.Dir = vm.dir
	// The kernel and all the guest's processes are host processes.
	// Put them in their own process group so that Close can kill
	// them all. Also have the host kernel kill the guest if this
	// process dies without calling Close, such as on a test timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return err
	}
	vm.cmd = cmd
	if vm.cpu != nil {
		// The guest's userspace processes are forked later and
		// inherit this.
		if err := unix.SchedSetaffinity(cmd.Process.Pid, vm.cpu); err != nil {
			vm.logf("pinning guest: %v", err)
		}
	}
	pidfd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if err != nil {
		vm.kill()
		return fmt.Errorf("pidfd_open: %w", err)
	}
	vm.pidfd = pidfd
	vm.t0 = time.Now()
	go vm.manage()
	return nil
}

func (vm *VM) kill() {
	if vm.cmd != nil && vm.cmd.Process != nil {
		unix.Kill(-vm.cmd.Process.Pid, unix.SIGKILL)
	}
}

// Close kills the guest and releases its resources.
func (vm *VM) Close() error {
	vm.stopOnce.Do(func() { close(vm.stop) })
	vm.mu.Lock()
	started := vm.started
	vm.mu.Unlock()
	if started && vm.cmd != nil {
		vm.kill()
		<-vm.done
		vm.cmd.Wait()
		// The guest's userspace processes are children of the
		// kernel process and may outlive it briefly.
		vm.kill()
	}
	for _, fd := range []*int{&vm.ttListen, &vm.tt, &vm.pidfd} {
		if *fd >= 0 {
			unix.Close(*fd)
			*fd = -1
		}
	}
	for _, n := range vm.nics {
		n.dev.close()
	}
	os.RemoveAll(vm.dir)
	return nil
}

// Done returns a channel that is closed when the guest has exited
// or the VM was closed.
func (vm *VM) Done() <-chan struct{} { return vm.done }

// Err reports why the guest stopped being managed.
// It is only meaningful once Done is closed.
func (vm *VM) Err() error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.err
}

// Stats returns the VM's activity counters.
func (vm *VM) Stats() Stats {
	return Stats{
		Runs:      int(vm.runs.Load()),
		TimerRuns: int(vm.timerRuns.Load()),
		IRQRuns:   int(vm.irqRuns.Load()),
		TTMsgs:    int(vm.ttMsgs.Load()),
	}
}

// now returns the bubble's clock on the guest's timeline, in
// nanoseconds since the guest booted.
func (vm *VM) now() uint64 {
	return uint64(time.Since(vm.t0))
}

// pinCPU returns the CPU that all VMs are pinned to, which is the
// highest numbered one this process is allowed to run on. It returns
// nil if that can't be determined.
var pinCPU = sync.OnceValue(func() *unix.CPUSet {
	var allowed unix.CPUSet
	if err := unix.SchedGetaffinity(0, &allowed); err != nil {
		return nil
	}
	for cpu := len(allowed)*64 - 1; cpu >= 0; cpu-- {
		if allowed.IsSet(cpu) {
			var set unix.CPUSet
			set.Set(cpu)
			return &set
		}
	}
	return nil
})

var errGuestExited = errors.New("uml: guest exited")

// manage is the VM's manager goroutine. It alternates between letting
// the guest run, during which it blocks in real system calls so the
// bubble cannot go idle, and waiting on the bubble's fake clock for
// the guest's next deadline.
func (vm *VM) manage() {
	defer close(vm.done)
	if vm.cpu != nil {
		// Pin this goroutine's thread to the guest's CPU. The
		// thread is never unlocked, so the runtime destroys it,
		// along with its affinity, when this goroutine exits.
		runtime.LockOSThread()
		if err := unix.SchedSetaffinity(0, vm.cpu); err != nil {
			vm.logf("pinning manager: %v", err)
		}
	}
	err := vm.manageLoop()
	vm.mu.Lock()
	vm.err = err
	vm.mu.Unlock()
	if err != nil && err != errGuestExited {
		vm.logf("manager exiting: %v", err)
	}
}

func (vm *VM) manageLoop() error {
	for {
		if err := vm.runUntilWait(); err != nil {
			return err
		}
		if err := vm.waitAndGrant(); err != nil {
			return err
		}
	}
}

// runUntilWait services the guest's sockets while it is executing.
// It returns when the guest reports that it is waiting for its
// requested run time.
func (vm *VM) runUntilWait() error {
	var pfds []unix.PollFd
	for {
		pfds = pfds[:0]
		add := func(fd int) {
			pfds = append(pfds, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN})
		}
		// The order here matters: device sockets are checked
		// before the time-travel socket, because the guest is
		// single threaded and anything it sent to a device was
		// sent before it sent WAIT.
		for _, n := range vm.nics {
			if n.dev.fd >= 0 {
				add(n.dev.fd)
			} else {
				add(n.dev.listenFD)
			}
		}
		if vm.tt >= 0 {
			add(vm.tt)
		} else {
			add(vm.ttListen)
		}
		add(vm.pidfd)

		if _, err := unix.Poll(pfds, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("poll: %w", err)
		}
		i := 0
		for _, n := range vm.nics {
			ready := pfds[i].Revents != 0
			i++
			if !ready {
				continue
			}
			if n.dev.fd < 0 {
				fd, err := accept(n.dev.listenFD)
				if err != nil {
					return err
				}
				n.dev.fd = fd
				continue
			}
			if err := n.dev.handleMsg(); err != nil {
				if err == io.EOF {
					return errGuestExited
				}
				return fmt.Errorf("vhost-user: %w", err)
			}
		}
		if pfds[i].Revents != 0 {
			if vm.tt < 0 {
				fd, err := accept(vm.ttListen)
				if err != nil {
					return err
				}
				vm.tt = fd
			} else {
				m, err := vm.readTT()
				if err != nil {
					return err
				}
				if err := vm.handleTT(m); err != nil {
					return err
				}
				if m.op == ttWait {
					return nil
				}
			}
			continue
		}
		i++
		if pfds[i].Revents != 0 {
			return errGuestExited
		}
	}
}

func accept(listenFD int) (int, error) {
	for {
		fd, _, err := unix.Accept4(listenFD, unix.SOCK_CLOEXEC)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return -1, fmt.Errorf("accept: %w", err)
		}
		return fd, nil
	}
}

func (vm *VM) readTT() (ttMsg, error) {
	var buf [ttMsgSize]byte
	if err := readFull(vm.tt, buf[:]); err != nil {
		if err == io.EOF {
			return ttMsg{}, errGuestExited
		}
		return ttMsg{}, fmt.Errorf("time-travel read: %w", err)
	}
	m := decodeTTMsg(buf[:])
	vm.ttMsgs.Add(1)
	if vm.cfg.Trace {
		vm.logf("<- %v (bubble=%d)", m, vm.now())
	}
	return m, nil
}

func (vm *VM) writeTT(m ttMsg) error {
	if vm.cfg.Trace {
		vm.logf("-> %v", m)
	}
	if err := writeFull(vm.tt, m.encode()); err != nil {
		return fmt.Errorf("time-travel write: %w", err)
	}
	return nil
}

// handleTT handles a message from the guest and acknowledges it.
func (vm *VM) handleTT(m ttMsg) error {
	ack := ttMsg{op: ttACK, seq: m.seq}
	switch m.op {
	case ttStart:
		return vm.ackStart(ack)
	case ttGetTOD:
		// The guest computes its wall clock as this value minus
		// its current time-travel time, so this must be the wall
		// time right now, not at boot.
		ack.time = uint64(time.Now().UnixNano())
	case ttGet:
		t := max(vm.now(), vm.minTime)
		vm.minTime = t
		ack.time = t
	case ttRequest:
		vm.req = m.time
		vm.reqValid = true
	case ttUpdate:
		// The guest is about to do something visible to the
		// outside world, such as transmit a frame, and is saying
		// what time it is. If its clock has run ahead of the
		// bubble's, let the bubble catch up first, so that the
		// Go side sees the guest's actions at the right time.
		if now := vm.now(); m.time > now && m.time < ttInfinity {
			if _, err := vm.sleep(time.Duration(m.time-now), false); err != nil {
				return err
			}
		}
		vm.minTime = max(vm.minTime, m.time)
	case ttWait:
		// The protocol says the time field of WAIT is ignored, and
		// the kernel sends -1. A kernel that sends its clock there
		// instead gives an exact value where minTime was only an
		// upper bound.
		if m.time < ttInfinity {
			vm.minTime = m.time
		}
	case ttBroadcast:
	default:
		return fmt.Errorf("unexpected time-travel message %v", m)
	}
	return vm.writeTT(ack)
}

// ackStart acknowledges the START message.
//
// The kernel unconditionally reads two file descriptors out of the
// control message accompanying this ACK (a shared memory area and a
// log file), and uses whatever is on its stack if none were sent. So
// send it two harmless ones: a zeroed memfd, which it rejects as
// having the wrong version, and /dev/null.
func (vm *VM) ackStart(ack ttMsg) error {
	memfd, err := unix.MemfdCreate("uml-tt-shm", unix.MFD_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(memfd)
	if err := unix.Ftruncate(memfd, 2*4096); err != nil {
		return err
	}
	null, err := unix.Open("/dev/null", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(null)
	if vm.cfg.Trace {
		vm.logf("-> %v (with fds)", ack)
	}
	return unix.Sendmsg(vm.tt, ack.encode(), unix.UnixRights(memfd, null), nil, 0)
}

// expectACK reads from the time-travel socket until the guest
// acknowledges a message this side sent.
func (vm *VM) expectACK() error {
	m, err := vm.readTT()
	if err != nil {
		return err
	}
	if m.op != ttACK {
		return fmt.Errorf("expected ACK from guest, got %v", m)
	}
	return nil
}

// grant tells the guest to run at time t, which must be exactly the
// time it most recently requested.
func (vm *VM) grant(t uint64) error {
	if vm.slack > 0 {
		if err := vm.writeTT(ttMsg{op: ttFreeUntil, time: t + vm.slack}); err != nil {
			return err
		}
		if err := vm.expectACK(); err != nil {
			return err
		}
	}
	if err := vm.writeTT(ttMsg{op: ttRun, time: t}); err != nil {
		return err
	}
	if err := vm.expectACK(); err != nil {
		return err
	}
	vm.minTime = max(vm.minTime, t+vm.slack)
	vm.reqValid = false
	vm.runs.Add(1)
	return nil
}

// sleep blocks durably in the bubble for d, or until the VM is closed
// or, if wakeable, until a device has input for the guest.
// It reports whether the full duration elapsed.
func (vm *VM) sleep(d time.Duration, wakeable bool) (elapsed bool, err error) {
	var timerC <-chan time.Time
	if d >= 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timerC = t.C
	}
	var wake chan struct{}
	if wakeable {
		wake = vm.wake
	}
	select {
	case <-timerC:
		return true, nil
	case <-wake:
		return false, nil
	case <-vm.stop:
		return false, errGuestExited
	}
}

// waitAndGrant is called when the guest is waiting. It blocks in the
// bubble until either the guest's requested time arrives or a device
// has an interrupt to deliver, and then lets the guest run again.
func (vm *VM) waitAndGrant() error {
	for {
		now := vm.now()
		if vm.reqValid && vm.req <= now {
			vm.timerRuns.Add(1)
			return vm.grant(vm.req)
		}
		if vm.irqPending() {
			// The guest's clock may have run ahead of the
			// bubble's by up to the slack. Let the bubble catch
			// up before telling the guest what time it is.
			if vm.minTime > now {
				if _, err := vm.sleep(time.Duration(vm.minTime-now), false); err != nil {
					return err
				}
				continue
			}
			requested, err := vm.injectIRQs()
			if err != nil {
				return err
			}
			if requested {
				vm.irqRuns.Add(1)
				return vm.grant(vm.req)
			}
			// Nothing could be delivered right now. Fall
			// through to wait for the guest's timer.
		}
		d := time.Duration(-1)
		if vm.reqValid && vm.req < ttInfinity {
			d = time.Duration(vm.req - now)
		}
		if _, err := vm.sleep(d, true); err != nil {
			return err
		}
	}
}

func (vm *VM) irqPending() bool {
	for _, n := range vm.nics {
		if n.irqPending() {
			return true
		}
	}
	return false
}

// injectIRQs delivers pending device interrupts to the waiting guest.
// It reports whether the guest responded by requesting to run now.
func (vm *VM) injectIRQs() (requested bool, err error) {
	calls := 0
	for _, n := range vm.nics {
		c, err := n.inject()
		if err != nil {
			return false, err
		}
		calls += c
	}
	if calls == 0 {
		return false, nil
	}
	// The guest's SIGIO handler reads each call message, and for
	// each one sends GET, followed, the first time only, by a
	// REQUEST to run now. This side must not send RUN until that
	// handler is finished, or the handler would swallow the RUN
	// while waiting for an ACK. Devices always send their call
	// messages in pairs in a single write, so the end of the
	// handler is recognizable: any REQUEST comes before the final
	// GET.
	sawRequest := false
	for gets := 0; gets < calls; {
		m, err := vm.readTT()
		if err != nil {
			return false, err
		}
		switch m.op {
		case ttGet:
			gets++
		case ttRequest:
			sawRequest = true
		default:
			return false, fmt.Errorf("unexpected %v while injecting interrupt", m)
		}
		if err := vm.handleTT(m); err != nil {
			return false, err
		}
	}
	return sawRequest, nil
}

func readFull(fd int, p []byte) error {
	for len(p) > 0 {
		n, err := unix.Read(fd, p)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			if err == unix.ECONNRESET {
				return io.EOF
			}
			return err
		}
		if n == 0 {
			return io.EOF
		}
		p = p[n:]
	}
	return nil
}

func writeFull(fd int, p []byte) error {
	for len(p) > 0 {
		n, err := unix.Write(fd, p)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}
