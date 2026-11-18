# Network simulations on fake time

This directory shows how to run netstack, and real Linux kernels, inside a Go
[`testing/synctest`](https://pkg.go.dev/testing/synctest) bubble. In a bubble
the clock is fake. It does not advance while any goroutine can run, and when
every goroutine is blocked it jumps straight to the next timer. A test that
simulates hours of network time finishes in seconds, and no real sleep is
involved.

The demos use that to measure TCP behavior that is slow to observe in real
time:

-   A bulk transfer over a link with 360 ms of round-trip delay and 1% packet
    loss. 128 MiB takes about 30 minutes of simulated time. The netstack version
    of the test runs in about 2 seconds, and the Linux version in about 25
    seconds.
-   The same transfer between two Linux kernels, over the same simulated link,
    as a reference to compare netstack against.
-   Linux TCP keepalives: a connection that sits idle for hours, then loses its
    peer. The test covers more than 6 hours of simulated time in a few seconds.

## What is here

| Path         | Contents                                                      |
| ------------ | ------------------------------------------------------------- |
| `xfer/`      | The bulk transfer tests: `TestNetstack` and `TestLinux`.      |
| `simnet/`    | A simulated point-to-point link with delay, jitter, and loss. |
| `uml/`       | Runs Linux kernels whose clocks follow the bubble's clock.    |
| `umltest/`   | Test helpers that boot Linux guests.                          |
| `guestinit/` | The init program of the Linux guests.                         |
| `initramfs/` | Writes the initramfs image that the guests boot from.         |
| `kernel/`    | The configuration and build script for the guest kernel.      |

## How it works

### Netstack

Netstack is ordinary Go code, so it runs in a bubble directly. Its timers use
the bubble's clock. The test creates two stacks, connects their link endpoints
through `simnet.Link`, and moves data over a TCP connection between them.

This needs the change to `pkg/sleep` and `pkg/syncevent` that this branch is
based on. Without it, the idle goroutines of a stack do not look blocked to
synctest, and the clock of the bubble never advances.

### Linux

The Linux guests are
[User-Mode Linux](https://docs.kernel.org/virt/uml/user_mode_linux_howto_v2.html)
kernels: the Linux kernel built as an ordinary Linux program. Each guest is a
child process of the test. It runs unmodified Linux binaries on a real Linux TCP
stack.

User-Mode Linux has a mode named `time-travel=ext`. In this mode the kernel
never reads the host clock, and it behaves as if its CPU is infinitely fast. It
asks an external scheduler, over a Unix socket, for permission to advance its
clock. The `uml` package implements that scheduler on top of the bubble's clock:

-   When the guest is idle, its next timer becomes a Go timer in the bubble.
    The bubble's clock can then jump to whichever of Go or Linux has work next.
-   While the guest runs, the goroutine that manages it is in a blocking system
    call. Synctest does not consider that goroutine to be blocked, so the clock
    cannot advance.

The guest's network interface is a virtio-net device over vhost-user, which the
`uml` package also implements. The test sends and receives raw Ethernet frames.
Queue notifications and interrupts are messages that the guest waits for, so a
guest never runs at the same time as the code that handles its frames.

The guest programs do not know about any of this. They see a normal Linux
system whose wall clock matches the bubble's clock, which starts at midnight
UTC on 2000-01-01.

## Requirements

-   A Linux x86-64 host.
-   For the Linux guests: a Linux kernel source tree and a C compiler. The
    demos were developed against a mainline tree from the 7.4 merge window.

The netstack test needs neither.

## Running the netstack transfer

```
bazel test //demos/synctest/xfer:netstack_xfer_test \
    --test_arg=-test.v --test_output=streamed \
    --test_env=XFER_BYTES=134217728 --test_env=XFER_CC=cubic
```

The results are in the test log, which Go only prints with `-test.v`:

```
sender: retransmits=1006 fast_retransmits=763 sack_recoveries=763 timeouts=12 slow_start_retransmits=12
sender: rto=360ms rtt=360ms rttvar=0s reorder_seen=true
sender, sampled: avg_cwnd=18.3 open=83.9% disorder=0.0% fast_recovery=0.0% sack_recovery=15.8% rto_recovery=0.2%
link: 180ms one-way delay, up to 0s jitter, 1.00% loss; 155122 frames sent, 1537 dropped (0.99%); 61236 from the receiver, 93886 from the sender
RESULT netstack: received 134217728 bytes in 32m5.28s of virtual time (70 kB/s), 2.339s of real time (823x)
```

## Running the Linux tests

Build the guest kernel once. The build is out of tree and does not modify the
kernel source directory:

```
demos/synctest/kernel/build.sh ~/src/linux /tmp/uml-build
```

Then give the tests the path of the resulting `linux` binary:

```
bazel test //demos/synctest/xfer:linux_xfer_test \
    --test_arg=-test.v --test_output=streamed \
    --test_env=UML_KERNEL=/tmp/uml-build/linux \
    --test_env=XFER_BYTES=134217728 --test_env=XFER_CC=cubic
```

```
sender: RESULT tcpinfo cc=cubic retrans=923 rtt=364.774ms rttvar=8.01ms cwnd=15 ssthresh=15 mss=1448
sender: RESULT tcpinfo2 rto=570ms total_rto=0 total_rto_recoveries=0 total_rto_time=0ms dsack_dups=6 reord_seen=0
sender: RESULT sampled avg_cwnd=18.9 open=82.3% disorder=1.0% cwr=0.0% recovery=16.7% loss=0.0%
link: 180ms one-way delay, up to 0s jitter, 1.00% loss; 124638 frames sent, 1234 dropped (0.99%); 30946 from the receiver, 93692 from the sender
RESULT linux: received 134217728 bytes in 29m55.373s of virtual time (75 kB/s), 23.283s of real time (77x)
```

The tests that need a kernel skip themselves when `UML_KERNEL` is not set. They
start child processes and use `/dev/shm`, so their targets have the `local` tag
and do not run in the Bazel sandbox.

The `uml` package has tests of its own, which are also small examples of its
API:

```
bazel test //demos/synctest/uml:uml_test \
    --test_arg=-test.v --test_output=streamed \
    --test_env=UML_KERNEL=/tmp/uml-build/linux
```

-   `TestSleep`: a guest sleeps for 24 hours while Go timers fire in between.
-   `TestEther`: a guest and the test exchange Ethernet frames, each on its own
    timer, and check that their clocks agree.
-   `TestTCPKeepAlive`: two guests hold an idle TCP connection with
    `SO_KEEPALIVE`. The test cuts the cable after 5 hours and watches the
    keepalive probes on the wire until the kernel gives up.

## Settings

The transfer tests read these environment variables. With Bazel, pass each one
with `--test_env`.

| Variable        | Meaning                                              | Default       |
| --------------- | ---------------------------------------------------- | ------------- |
| `XFER_BYTES`    | Bytes to send.                                       | 16 MiB        |
| `XFER_DELAY`    | One-way delay of the link.                           | `180ms`       |
| `XFER_LOSS`     | Probability that a frame is lost, in each direction. | `0.01`        |
| `XFER_JITTER`   | Extra random one-way delay per frame, up to this.    | `0s`          |
| `XFER_CC`       | Congestion control of the sender: `cubic`, `reno`.   | stack default |
| `XFER_PKTTRACE` | `from,to`: log each TCP segment in this time window. | off           |

`TestNetstack` also reads:

| Variable    | Meaning                                         | Default       |
| ----------- | ----------------------------------------------- | ------------- |
| `XFER_RACK` | `1` enables RACK loss detection, `0` disables it. | stack default |

`TestLinux` also reads:

| Variable        | Meaning                                                    |
| --------------- | ---------------------------------------------------------- |
| `UML_KERNEL`    | Path of the guest kernel. Required.                        |
| `XFER_SYSCTLS`  | Sysctls for the sender's kernel, separated by commas.      |
| `XFER_RSYSCTLS` | Sysctls for the receiver's kernel.                         |
| `UML_SLACK`     | See "Accuracy and speed" below.                            |
| `UML_VERBOSE`   | Log the boot messages of the guest kernels.                |
| `UML_TRACE`     | Log each message of the time-travel protocol.              |

For example, this runs the Linux transfer with SACK off in the sender's kernel:

```
--test_env=XFER_SYSCTLS=net.ipv4.tcp_sack=0
```

The guest kernel has the CUBIC, Reno, and BBR congestion control algorithms;
netstack has CUBIC and Reno. Each guest is a separate kernel, so the two ends
of a connection can have different settings.

## Looking at packets

`XFER_PKTTRACE` logs every TCP segment that goes on the link in a window of
simulated time, and every frame that the link drops. Sequence numbers are in
units of one segment:

```
--test_env=XFER_PKTTRACE=300s,303s
```

```
pkt   300.602s sndr seq=912142.0 ack=27335.7 len=1448 win=2048 sack=[]
pkt   300.782s rcvr seq=27335.7 ack=912143.0 len=0 win=16364 sack=[]
pkt   301.320s DROP seq=912154.2 ack=27335.7 len=1448 win=2048 sack=[]
```

The traces work the same way for netstack and for Linux, so the two can be
compared segment by segment.

## Accuracy and speed

A simulation is not a real network. These points matter when you read the
results:

-   **The CPU is infinitely fast.** No time passes while a stack processes a
    packet. The link has delay and loss but no bandwidth limit and no queue, so
    throughput depends only on how TCP reacts to delay and loss.
-   **The link is deterministic unless you add jitter.** With no jitter, every
    round trip takes exactly the same time, and the variance that TCP measures
    goes to zero. Real paths are not like that. Use `XFER_JITTER` to check that
    a result does not depend on it.
-   **Netstack gets a clock that always ticks.** In a bubble, everything that a
    stack does in response to one packet happens at one instant. Segments sent
    in one burst would have equal timestamps, and a timer due at the instant an
    ACK arrives would race with it. `TestNetstack` gives each stack a clock
    that advances one nanosecond per reading to avoid both.
-   **Loss is random and seeded.** Each direction of the link has its own
    random source with a fixed seed. Netstack runs are still not identical from
    run to run, because the Go scheduler orders goroutines differently. Expect
    results to vary by a few percent.
-   **A Linux guest's clock can run slightly ahead.** The guest kernel moves
    its clock forward 256 ns each time something reads it. `uml.Config.Slack`
    is how far the guest may run ahead of the bubble before it must ask. The
    default is 100 µs. A larger value is faster, but a frame for a guest that
    ran recently can then arrive up to that much late.

Linux tests are slower than netstack tests because each frame wakes a guest,
and each wake-up is several round trips between host processes. The `uml`
package pins a guest and the goroutine that manages it to one CPU, which makes
those round trips several times cheaper.

## Writing a test of your own

A netstack test needs nothing special: create the stacks inside
`synctest.Test` and connect their link endpoints with a `simnet.Link`.
`xfer/netstack_test.go` is the example.

A Linux guest takes a few lines:

```go
synctest.Test(t, func(t *testing.T) {
    g := umltest.NewGuest(t, kernel, initrd, "name", "GUEST_MODE=tcp-sink", "GUEST_IP=10.0.0.1/24")
    nic, err := g.AddNIC(mac, func(frame []byte) {
        // The guest transmitted frame.
    })
    ...
    g.Start()
    nic.Send(frame) // Deliver a frame to the guest.
    <-g.Done()      // The guest powered off.
})
```

The `GUEST_` variables select what `guestinit` does in the guest; see
`guestinit/main.go`. To run a different program, add it to the initramfs in
`umltest.Initrd`. A guest reports results on its console, which
`Guest.Lines` returns.

Find the kernel and build the initramfs before you enter the bubble, as the
existing tests do. A VM must be created, started, and closed inside it.

## Limits

-   Only Linux x86-64 hosts are supported.
-   User-Mode Linux has one CPU.
-   A guest has no disk. Its root file system is the initramfs.
-   The tests cannot control a guest while it runs, other than through its
    network interface. `guestinit` gets its instructions from the kernel
    command line.
-   Kernels up to at least 7.4-rc1 use two file descriptors from the reply to
    the first time-travel message without checking that they were sent. The
    `uml` package always sends two, so this has no effect on the demos.
