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
	"errors"
	"fmt"
	"io"

	"golang.org/x/sys/unix"
)

// Vhost-user protocol constants, as used by arch/um/drivers/virtio_uml.c.
const (
	vuFlagVersion   = 1
	vuFlagReply     = 1 << 2
	vuFlagNeedReply = 1 << 3

	vuFProtocolFeatures = 30 // feature bit

	vuProtoReplyAck = 3
	vuProtoSlaveReq = 5
	vuProtoConfig   = 9
	vuProtoInband   = 14

	vuGetFeatures         = 1
	vuSetFeatures         = 2
	vuSetOwner            = 3
	vuSetMemTable         = 5
	vuSetVringNum         = 8
	vuSetVringAddr        = 9
	vuSetVringBase        = 10
	vuGetVringBase        = 11
	vuSetVringKick        = 12
	vuSetVringCall        = 13
	vuGetProtocolFeatures = 15
	vuSetProtocolFeatures = 16
	vuGetQueueNum         = 17
	vuSetVringEnable      = 18
	vuSetSlaveReqFD       = 21
	vuGetConfig           = 24
	vuSetConfig           = 25
	vuVringKick           = 35

	vuSlaveVringCall = 4

	vuHdrSize = 12
)

// Virtio constants.
const (
	virtioIDNet = 1

	virtioFVersion1 = 32

	vringDescFNext  = 1
	vringDescFWrite = 2

	vringAvailFNoInterrupt = 1
)

// vring is the device side of one split virtqueue.
type vring struct {
	num       uint32
	desc      uint64 // guest address of the descriptor table
	avail     uint64 // guest address of the available ring
	used      uint64 // guest address of the used ring
	lastAvail uint16 // next available ring entry to consume
	usedIdx   uint16 // next used ring entry to fill
	enabled   bool
}

// vuDev is a vhost-user device backend serving one virtio device of
// one guest. All of its methods run on the VM's manager goroutine.
//
// It requires the in-band notification protocol extension, in which
// the guest's queue kicks are messages on the vhost-user socket that
// the guest waits for a reply to, and interrupts to the guest are
// messages rather than eventfd writes. This makes all communication
// with the guest synchronous, which is what lets it be simulated
// deterministically. It also means the guest is stopped whenever this
// code touches the virtqueues, so no memory barriers are needed.
type vuDev struct {
	vm       *VM
	sockPath string
	listenFD int
	fd       int // connected vhost-user socket, or -1
	slaveFD  int // pipe for sending messages to the guest, or -1

	features uint64 // virtio features offered
	config   []byte // virtio config space
	vqs      []vring

	mem     []byte // mmap of the guest's physical memory
	memBase uint64 // guest address of mem[0]

	onKick func(vq int) error
}

func (d *vuDev) close() {
	for _, fd := range []*int{&d.listenFD, &d.fd, &d.slaveFD} {
		if *fd >= 0 {
			unix.Close(*fd)
			*fd = -1
		}
	}
	if d.mem != nil {
		unix.Munmap(d.mem)
		d.mem = nil
	}
}

// guest returns the n bytes of guest memory at addr.
func (d *vuDev) guest(addr uint64, n uint32) ([]byte, error) {
	if addr < d.memBase || addr-d.memBase+uint64(n) > uint64(len(d.mem)) {
		return nil, fmt.Errorf("guest address %#x+%d is outside shared memory", addr, n)
	}
	off := addr - d.memBase
	return d.mem[off : off+uint64(n) : off+uint64(n)], nil
}

// handleMsg reads and handles one vhost-user message from the guest.
func (d *vuDev) handleMsg() error {
	var hdr [vuHdrSize]byte
	oob := make([]byte, unix.CmsgSpace(4*8))
	n, oobn, _, _, err := unix.Recvmsg(d.fd, hdr[:], oob, 0)
	if err != nil {
		if err == unix.ECONNRESET {
			return io.EOF
		}
		return err
	}
	if n == 0 {
		return io.EOF
	}
	if err := readFull(d.fd, hdr[n:]); err != nil {
		return err
	}
	var fds []int
	if oobn > 0 {
		cmsgs, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return err
		}
		for _, c := range cmsgs {
			got, err := unix.ParseUnixRights(&c)
			if err != nil {
				return err
			}
			fds = append(fds, got...)
		}
	}
	// Any file descriptor that a handler wants to keep is removed
	// from fds by setting it to -1.
	defer func() {
		for _, fd := range fds {
			if fd >= 0 {
				unix.Close(fd)
			}
		}
	}()

	req := binary.LittleEndian.Uint32(hdr[0:])
	flags := binary.LittleEndian.Uint32(hdr[4:])
	size := binary.LittleEndian.Uint32(hdr[8:])
	if size > 4096 {
		return fmt.Errorf("oversized vhost-user message: request %d, size %d", req, size)
	}
	payload := make([]byte, size)
	if err := readFull(d.fd, payload); err != nil {
		return err
	}
	u64 := func() uint64 {
		if len(payload) < 8 {
			return 0
		}
		return binary.LittleEndian.Uint64(payload)
	}
	// vringState decodes a struct vhost_user_vring_state.
	vringState := func() (vq *vring, idx int, num uint32, err error) {
		if len(payload) < 8 {
			return nil, 0, 0, errors.New("short vring state")
		}
		idx = int(binary.LittleEndian.Uint32(payload[0:]))
		num = binary.LittleEndian.Uint32(payload[4:])
		if idx >= len(d.vqs) {
			return nil, 0, 0, fmt.Errorf("vring index %d out of range", idx)
		}
		return &d.vqs[idx], idx, num, nil
	}

	switch req {
	case vuGetFeatures:
		return d.replyU64(req, d.features)
	case vuGetProtocolFeatures:
		return d.replyU64(req, 1<<vuProtoReplyAck|1<<vuProtoSlaveReq|1<<vuProtoConfig|1<<vuProtoInband)
	case vuGetQueueNum:
		return d.replyU64(req, uint64(len(d.vqs)))
	case vuSetOwner, vuSetFeatures, vuSetProtocolFeatures, vuSetConfig, vuSetVringCall, vuSetVringKick:
		// Nothing to do.
	case vuSetSlaveReqFD:
		if len(fds) != 1 {
			return errors.New("SET_SLAVE_REQ_FD without a file descriptor")
		}
		d.slaveFD, fds[0] = fds[0], -1
	case vuGetConfig:
		// The payload is a struct vhost_user_config followed by
		// space for the config bytes.
		if len(payload) < 12 {
			return errors.New("short GET_CONFIG")
		}
		copy(payload[12:], d.config)
		return d.reply(req, payload)
	case vuSetMemTable:
		if err := d.setMemTable(payload, fds); err != nil {
			return err
		}
	case vuSetVringNum:
		vq, _, num, err := vringState()
		if err != nil {
			return err
		}
		vq.num = num
	case vuSetVringBase:
		vq, _, num, err := vringState()
		if err != nil {
			return err
		}
		vq.lastAvail = uint16(num)
	case vuSetVringEnable:
		vq, _, num, err := vringState()
		if err != nil {
			return err
		}
		vq.enabled = num != 0
	case vuSetVringAddr:
		// The payload is a struct vhost_user_vring_addr.
		if len(payload) < 40 {
			return errors.New("short SET_VRING_ADDR")
		}
		idx := binary.LittleEndian.Uint32(payload[0:])
		if int(idx) >= len(d.vqs) {
			return fmt.Errorf("vring index %d out of range", idx)
		}
		vq := &d.vqs[idx]
		vq.desc = binary.LittleEndian.Uint64(payload[8:])
		vq.used = binary.LittleEndian.Uint64(payload[16:])
		vq.avail = binary.LittleEndian.Uint64(payload[24:])
		vq.usedIdx = 0
	case vuVringKick:
		_, idx, _, err := vringState()
		if err != nil {
			return err
		}
		if d.onKick != nil {
			if err := d.onKick(idx); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported vhost-user request %d (payload %d bytes, value %#x)", req, size, u64())
	}
	if flags&vuFlagNeedReply != 0 {
		return d.replyU64(req, 0)
	}
	return nil
}

func (d *vuDev) reply(req uint32, payload []byte) error {
	b := make([]byte, vuHdrSize+len(payload))
	binary.LittleEndian.PutUint32(b[0:], req)
	binary.LittleEndian.PutUint32(b[4:], vuFlagVersion|vuFlagReply)
	binary.LittleEndian.PutUint32(b[8:], uint32(len(payload)))
	copy(b[vuHdrSize:], payload)
	return writeFull(d.fd, b)
}

func (d *vuDev) replyU64(req uint32, v uint64) error {
	var p [8]byte
	binary.LittleEndian.PutUint64(p[:], v)
	return d.reply(req, p[:])
}

// setMemTable handles SET_MEM_TABLE by mapping the guest's memory.
func (d *vuDev) setMemTable(payload []byte, fds []int) error {
	if len(payload) < 8+32 || len(fds) < 1 {
		return errors.New("short SET_MEM_TABLE")
	}
	if n := binary.LittleEndian.Uint32(payload); n != 1 {
		return fmt.Errorf("SET_MEM_TABLE with %d regions; only 1 supported", n)
	}
	r := payload[8:]
	guestAddr := binary.LittleEndian.Uint64(r[0:])
	size := binary.LittleEndian.Uint64(r[8:])
	mmapOff := binary.LittleEndian.Uint64(r[24:])

	// mmap offsets must be page aligned, so map from the start of
	// the page containing the region.
	slop := mmapOff & 4095
	if d.mem != nil {
		unix.Munmap(d.mem)
		d.mem = nil
	}
	mem, err := unix.Mmap(fds[0], int64(mmapOff-slop), int(size+slop), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("mmap guest memory: %w", err)
	}
	d.mem = mem
	d.memBase = guestAddr - slop
	return nil
}

// desc is a decoded struct vring_desc.
type desc struct {
	addr  uint64
	len   uint32
	flags uint16
	next  uint16
}

func (d *vuDev) readDesc(vq *vring, i uint16) (desc, error) {
	if uint32(i) >= vq.num {
		return desc{}, fmt.Errorf("descriptor index %d out of range", i)
	}
	b, err := d.guest(vq.desc+16*uint64(i), 16)
	if err != nil {
		return desc{}, err
	}
	return desc{
		addr:  binary.LittleEndian.Uint64(b[0:]),
		len:   binary.LittleEndian.Uint32(b[8:]),
		flags: binary.LittleEndian.Uint16(b[12:]),
		next:  binary.LittleEndian.Uint16(b[14:]),
	}, nil
}

// availIdx returns the guest's available ring index.
func (d *vuDev) availIdx(vq *vring) (uint16, error) {
	b, err := d.guest(vq.avail+2, 2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

// wantsInterrupt reports whether the guest wants to be interrupted
// when buffers are used on vq.
func (d *vuDev) wantsInterrupt(vq *vring) bool {
	b, err := d.guest(vq.avail, 2)
	if err != nil {
		return false
	}
	return binary.LittleEndian.Uint16(b)&vringAvailFNoInterrupt == 0
}

// ready reports whether vq is set up and has buffers available.
func (d *vuDev) ready(vq *vring) bool {
	if !vq.enabled || vq.num == 0 || d.mem == nil {
		return false
	}
	idx, err := d.availIdx(vq)
	return err == nil && idx != vq.lastAvail
}

// popAvail takes the next buffer chain off vq's available ring.
// It returns the chain's head descriptor index and its buffers,
// split into those the device reads and those it writes.
func (d *vuDev) popAvail(vq *vring) (head uint16, readable, writable [][]byte, ok bool, err error) {
	if !d.ready(vq) {
		return 0, nil, nil, false, nil
	}
	b, err := d.guest(vq.avail+4+2*uint64(uint32(vq.lastAvail)%vq.num), 2)
	if err != nil {
		return 0, nil, nil, false, err
	}
	head = binary.LittleEndian.Uint16(b)
	i := head
	for n := uint32(0); ; n++ {
		if n > vq.num {
			return 0, nil, nil, false, errors.New("descriptor chain loop")
		}
		ds, err := d.readDesc(vq, i)
		if err != nil {
			return 0, nil, nil, false, err
		}
		buf, err := d.guest(ds.addr, ds.len)
		if err != nil {
			return 0, nil, nil, false, err
		}
		if ds.flags&vringDescFWrite != 0 {
			writable = append(writable, buf)
		} else {
			readable = append(readable, buf)
		}
		if ds.flags&vringDescFNext == 0 {
			break
		}
		i = ds.next
	}
	vq.lastAvail++
	return head, readable, writable, true, nil
}

// pushUsed returns a buffer chain to the guest, reporting that the
// device wrote n bytes into it.
func (d *vuDev) pushUsed(vq *vring, head uint16, n uint32) error {
	e, err := d.guest(vq.used+4+8*uint64(uint32(vq.usedIdx)%vq.num), 8)
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(e[0:], uint32(head))
	binary.LittleEndian.PutUint32(e[4:], n)
	vq.usedIdx++
	idx, err := d.guest(vq.used+2, 2)
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint16(idx, vq.usedIdx)
	return nil
}

// call interrupts the guest for each of the given virtqueues.
// It returns the number of call messages it sent, which is always
// even; see VM.injectIRQs for why.
func (d *vuDev) call(vqs ...int) (int, error) {
	if len(vqs) == 0 || d.slaveFD < 0 {
		return 0, nil
	}
	if len(vqs)%2 == 1 {
		vqs = append(vqs, vqs[0])
	}
	const msgSize = vuHdrSize + 8
	b := make([]byte, 0, len(vqs)*msgSize)
	for _, vq := range vqs {
		b = binary.LittleEndian.AppendUint32(b, vuSlaveVringCall)
		b = binary.LittleEndian.AppendUint32(b, vuFlagVersion)
		b = binary.LittleEndian.AppendUint32(b, 8)
		b = binary.LittleEndian.AppendUint32(b, uint32(vq))
		b = binary.LittleEndian.AppendUint32(b, 0)
	}
	// This must be a single write so that the guest handles all
	// the messages in one pass of its signal handler.
	n, err := unix.Write(d.slaveFD, b)
	if err != nil {
		return 0, fmt.Errorf("vhost-user call: %w", err)
	}
	if n != len(b) {
		return 0, errors.New("vhost-user call: short write")
	}
	return len(vqs), nil
}
