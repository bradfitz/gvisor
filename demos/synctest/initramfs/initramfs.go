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

// Package initramfs writes Linux initramfs images (cpio archives in
// the "newc" format) without needing root or any external tools.
package initramfs

import (
	"fmt"
	"io"
	"os"
)

// The file type bits of Entry.Mode.
const (
	ModeDir  = 0o040000
	ModeChar = 0o020000
	ModeReg  = 0o100000
)

// Entry is one file in the archive.
type Entry struct {
	Name         string // path without a leading slash
	Mode         uint32 // Unix mode, including the file type bits
	Data         []byte // contents, for regular files
	Major, Minor uint32 // device number, for device nodes
}

// Dir returns an Entry for a directory.
func Dir(name string) Entry { return Entry{Name: name, Mode: ModeDir | 0o755} }

// File returns an Entry for an executable regular file.
func File(name string, data []byte) Entry {
	return Entry{Name: name, Mode: ModeReg | 0o755, Data: data}
}

// CharDev returns an Entry for a character device node.
func CharDev(name string, major, minor uint32) Entry {
	return Entry{Name: name, Mode: ModeChar | 0o600, Major: major, Minor: minor}
}

// Write writes an archive containing entries to w.
func Write(w io.Writer, entries []Entry) error {
	cw := &countWriter{w: w}
	entries = append(entries[:len(entries):len(entries)], Entry{Name: "TRAILER!!!"})
	for i, e := range entries {
		name := e.Name + "\x00"
		nlink := 1
		if e.Mode&ModeDir != 0 {
			nlink = 2
		}
		// The header is the magic followed by 13 fields of 8 hex
		// digits each.
		fmt.Fprintf(cw, "070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
			i+1,    // inode
			e.Mode, // mode
			0, 0,   // uid, gid
			nlink,       // nlink
			0,           // mtime
			len(e.Data), // file size
			0, 0,        // device of the file system
			e.Major, e.Minor,
			len(name), // name size including NUL
			0)         // checksum
		io.WriteString(cw, name)
		cw.pad()
		cw.Write(e.Data)
		cw.pad()
	}
	return cw.err
}

// WriteFile writes an archive containing entries to the named file.
func WriteFile(path string, entries []Entry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := Write(f, entries); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// countWriter tracks the write offset so that entries can be padded
// to four byte boundaries, and remembers the first error.
type countWriter struct {
	w   io.Writer
	n   int
	err error
}

func (c *countWriter) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.w.Write(p)
	c.n += n
	c.err = err
	return n, err
}

func (c *countWriter) pad() {
	if r := c.n % 4; r != 0 {
		c.Write(make([]byte, 4-r))
	}
}
