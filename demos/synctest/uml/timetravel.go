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

// Package uml runs User-Mode Linux kernels whose clocks are slaved to
// the Go fake clock of a testing/synctest bubble.
//
// Each VM is a real Linux kernel (ARCH=um) running real, unmodified
// Linux userspace binaries. The kernel is started in its
// "time-travel=ext" mode, in which it has infinite CPU speed and never
// reads the host clock. Instead it asks an external scheduler (the
// "calendar") for permission to advance its clock. This package
// implements that calendar on top of the synctest fake clock: the
// guest kernel's next timer is turned into a Go timer in the bubble,
// and the guest is only told to run when the bubble's clock reaches it.
//
// While a guest is executing, its manager goroutine sits in a blocking
// system call, which synctest does not consider durably blocked, so the
// fake clock cannot advance. When the guest goes idle, the manager
// blocks on a bubbled timer, and the fake clock is free to jump to
// whichever of Go or Linux has work to do next.
package uml

import (
	"encoding/binary"
	"fmt"
)

// The time-travel protocol operations.
// They mirror enum um_timetravel_ops in include/uapi/linux/um_timetravel.h.
const (
	ttACK       = 0
	ttStart     = 1
	ttRequest   = 2
	ttWait      = 3
	ttGet       = 4
	ttUpdate    = 5
	ttRun       = 6
	ttFreeUntil = 7
	ttGetTOD    = 8
	ttBroadcast = 9
)

// ttMsgSize is the wire size of struct um_timetravel_msg.
const ttMsgSize = 16

// ttMsg is a struct um_timetravel_msg.
type ttMsg struct {
	op   uint32
	seq  uint32
	time uint64 // nanoseconds
}

func (m ttMsg) String() string {
	names := [...]string{"ACK", "START", "REQUEST", "WAIT", "GET", "UPDATE", "RUN", "FREE_UNTIL", "GET_TOD", "BROADCAST"}
	name := fmt.Sprintf("op%d", m.op)
	if int(m.op) < len(names) {
		name = names[m.op]
	}
	return fmt.Sprintf("%s(seq=%d, t=%d)", name, m.seq, int64(m.time))
}

func (m ttMsg) encode() []byte {
	b := make([]byte, ttMsgSize)
	binary.LittleEndian.PutUint32(b[0:], m.op)
	binary.LittleEndian.PutUint32(b[4:], m.seq)
	binary.LittleEndian.PutUint64(b[8:], m.time)
	return b
}

func decodeTTMsg(b []byte) ttMsg {
	return ttMsg{
		op:   binary.LittleEndian.Uint32(b[0:]),
		seq:  binary.LittleEndian.Uint32(b[4:]),
		time: binary.LittleEndian.Uint64(b[8:]),
	}
}
