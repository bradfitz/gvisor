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
	"encoding/binary"
	"fmt"
	"net"
	"path/filepath"
	"sync"
)

// Virtio-net constants.
const (
	virtioNetFMAC = 5

	virtioNetHdrLen = 12 // struct virtio_net_hdr_v1

	netRXQ = 0
	netTXQ = 1
)

// NIC is a virtio-net Ethernet interface of a VM.
// The Go side of it sends and receives raw Ethernet frames.
type NIC struct {
	vm  *VM
	dev *vuDev
	mac net.HardwareAddr

	// recv is called for each frame the guest transmits.
	recv func(frame []byte)

	mu         sync.Mutex
	rxq        [][]byte // frames waiting to be delivered to the guest
	txComplete bool     // guest is owed a transmit-complete interrupt

	// Counters, guarded by mu.
	txFrames, rxFrames int
}

// AddNIC adds an Ethernet interface to the VM. It must be called
// before Start. Interfaces appear in the guest as eth0, eth1, and so
// on, in the order they were added.
//
// The recv function is called with each Ethernet frame the guest
// transmits. It is called on the VM's manager goroutine while the
// guest is stopped waiting for it, so it must not block. It may call
// Send. The frame is only valid until recv returns.
func (vm *VM) AddNIC(mac net.HardwareAddr, recv func(frame []byte)) (*NIC, error) {
	n := &NIC{vm: vm, mac: mac, recv: recv}
	path := filepath.Join(vm.dir, fmt.Sprintf("net%d.sock", len(vm.nics)))
	lfd, err := listenUnix(path)
	if err != nil {
		return nil, err
	}
	n.dev = &vuDev{
		vm:       vm,
		sockPath: path,
		listenFD: lfd,
		fd:       -1,
		slaveFD:  -1,
		features: 1<<virtioNetFMAC | 1<<virtioFVersion1 | 1<<vuFProtocolFeatures,
		vqs:      make([]vring, 2),
		onKick:   n.onKick,
	}
	// The config space is a struct virtio_net_config, of which
	// only the MAC address is meaningful with the offered features.
	n.dev.config = make([]byte, 16)
	copy(n.dev.config, mac)
	vm.nics = append(vm.nics, n)
	return n, nil
}

// Send queues an Ethernet frame for delivery to the guest. It may be
// called from any goroutine in the bubble. The frame is delivered at
// the current virtual time, or up to Config.Slack later if the guest
// has just been running.
func (n *NIC) Send(frame []byte) {
	f := append([]byte(nil), frame...)
	n.mu.Lock()
	n.rxq = append(n.rxq, f)
	n.mu.Unlock()
	select {
	case n.vm.wake <- struct{}{}:
	default:
	}
}

// Counts returns the number of frames the guest has transmitted and
// received on this interface.
func (n *NIC) Counts() (tx, rx int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.txFrames, n.rxFrames
}

// onKick handles the guest's notification that it added buffers to
// a virtqueue.
func (n *NIC) onKick(vq int) error {
	if vq != netTXQ {
		// New receive buffers. Anything waiting for them is
		// delivered the next time the guest waits.
		return nil
	}
	d := n.dev
	q := &d.vqs[netTXQ]
	for {
		head, readable, _, ok, err := d.popAvail(q)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		var frame []byte
		for _, b := range readable {
			frame = append(frame, b...)
		}
		if err := d.pushUsed(q, head, 0); err != nil {
			return err
		}
		n.mu.Lock()
		n.txComplete = true
		n.txFrames++
		n.mu.Unlock()
		if len(frame) > virtioNetHdrLen && n.recv != nil {
			n.recv(frame[virtioNetHdrLen:])
		}
	}
	return nil
}

// irqPending reports whether the NIC has an interrupt to deliver.
func (n *NIC) irqPending() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.txComplete {
		return true
	}
	return len(n.rxq) > 0 && n.dev.ready(&n.dev.vqs[netRXQ])
}

// inject moves queued frames into the guest's receive buffers and
// interrupts the guest. It returns the number of call messages sent.
// The guest must be stopped.
func (n *NIC) inject() (calls int, err error) {
	d := n.dev
	n.mu.Lock()
	defer n.mu.Unlock()

	var vqs []int
	rq := &d.vqs[netRXQ]
	delivered := false
	for len(n.rxq) > 0 {
		head, _, writable, ok, err := d.popAvail(rq)
		if err != nil {
			return 0, err
		}
		if !ok {
			break
		}
		frame := n.rxq[0]
		n.rxq = n.rxq[1:]

		var hdr [virtioNetHdrLen]byte
		binary.LittleEndian.PutUint16(hdr[10:], 1) // num_buffers
		data := append(hdr[:], frame...)
		total := len(data)
		for _, w := range writable {
			c := copy(w, data)
			data = data[c:]
		}
		if len(data) > 0 {
			// The frame did not fit in the guest's buffer.
			// Drop it, but still return the buffer.
			total = 0
		} else {
			n.rxFrames++
		}
		if err := d.pushUsed(rq, head, uint32(total)); err != nil {
			return 0, err
		}
		delivered = true
	}
	if delivered && d.wantsInterrupt(rq) {
		vqs = append(vqs, netRXQ)
	}
	if n.txComplete {
		n.txComplete = false
		if d.wantsInterrupt(&d.vqs[netTXQ]) {
			vqs = append(vqs, netTXQ)
		}
	}
	return d.call(vqs...)
}
