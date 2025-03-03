// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package buildinfo provides access to information embedded in a Go binary
// about how it was built. This includes the Go toolchain version, and the
// set of modules used (for binaries built in module mode).
//
// Build information is available for the currently running binary in
// runtime/debug.ReadBuildInfo.

/*Copyright (C) 2022 Mandiant, Inc. All Rights Reserved.*/

package buildinfo

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/mandiant/GoReSym/runtime/debug"
	"github.com/mandiant/GoReSym/xcoff"
)

// Type alias for build info. We cannot move the types here, since
// runtime/debug would need to import this package, which would make it
// a much larger dependency.
type BuildInfo = debug.BuildInfo

var (
	// errUnrecognizedFormat is returned when a given executable file doesn't
	// appear to be in a known format, or it breaks the rules of that format,
	// or when there are I/O errors reading the file.
	errUnrecognizedFormat = errors.New("unrecognized file format")

	// errNotGoExe is returned when a given executable file is valid but does
	// not contain Go build information.
	errNotGoExe = errors.New("not a Go executable")

	// The build info blob left by the linker is identified by
	// a 16-byte header, consisting of buildInfoMagic (14 bytes),
	// the binary's pointer size (1 byte),
	// and whether the binary is big endian (1 byte).
	buildInfoMagic = []byte("\xff Go buildinf:")
)

// ReadFile returns build information embedded in a Go binary
// file at the given path. Most information is only available for binaries built
// with module support.
func ReadFile(name string) (info *BuildInfo, err error) {
	defer func() {
		if pathErr := (*fs.PathError)(nil); errors.As(err, &pathErr) {
			err = fmt.Errorf("could not read Go build info: %w", err)
		} else if err != nil {
			err = fmt.Errorf("could not read Go build info from %s: %w", name, err)
		}
	}()

	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(f)
}

// Read returns build information embedded in a Go binary file
// accessed through the given ReaderAt. Most information is only available for
// binaries built with module support.
func Read(r io.ReaderAt) (*BuildInfo, error) {
	vers, mod, err := readRawBuildInfo(r)
	if err != nil {
		return nil, err
	}
	bi := &BuildInfo{}
	if err := bi.UnmarshalText([]byte(mod)); err != nil {
		return nil, err
	}
	bi.GoVersion = vers
	return bi, nil
}

type exe interface {
	// ReadData reads and returns up to size bytes starting at virtual address addr.
	ReadData(addr, size uint64) ([]byte, error)

	// DataStart returns the virtual address of the segment or section that
	// should contain build information. This is either a specially named section
	// or the first writable non-zero data segment.
	DataStart() uint64
}

// readRawBuildInfo extracts the Go toolchain version and module information
// strings from a Go binary. On success, vers should be non-empty. mod
// is empty if the binary was not built with modules enabled.
func readRawBuildInfo(r io.ReaderAt) (vers, mod string, err error) {
	// Read the first bytes of the file to identify the format, then delegate to
	// a format-specific function to load segment and section headers.
	ident := make([]byte, 16)
	if n, err := r.ReadAt(ident, 0); n < len(ident) || err != nil {
		return "", "", errUnrecognizedFormat
	}

	var x exe
	switch {
	case bytes.HasPrefix(ident, []byte("\x7FELF")):
		f, err := elf.NewFile(r)
		if err != nil {
			return "", "", errUnrecognizedFormat
		}
		x = &elfExe{f}
	case bytes.HasPrefix(ident, []byte("MZ")):
		f, err := pe.NewFile(r)
		if err != nil {
			return "", "", errUnrecognizedFormat
		}
		x = &peExe{f}
	case bytes.HasPrefix(ident, []byte("\xFE\xED\xFA")) || bytes.HasPrefix(ident[1:], []byte("\xFA\xED\xFE")):
		f, err := macho.NewFile(r)
		if err != nil {
			return "", "", errUnrecognizedFormat
		}
		x = &machoExe{f}
	case bytes.HasPrefix(ident, []byte{0x01, 0xDF}) || bytes.HasPrefix(ident, []byte{0x01, 0xF7}):
		f, err := xcoff.NewFile(r)
		if err != nil {
			return "", "", errUnrecognizedFormat
		}
		x = &xcoffExe{f}
	default:
		return "", "", errUnrecognizedFormat
	}

	// Read the first 64kB of dataAddr to find the build info blob.
	// On some platforms, the blob will be in its own section, and DataStart
	// returns the address of that section. On others, it's somewhere in the
	// data segment; the linker puts it near the beginning.
	// See cmd/link/internal/ld.Link.buildinfo.
	dataAddr := x.DataStart()
	data, err := x.ReadData(dataAddr, 64*1024)
	if err != nil {
		return "", "", err
	}
	const (
		buildInfoAlign = 16
		buildInfoSize  = 32
	)
	for {
		i := bytes.Index(data, buildInfoMagic)
		if i < 0 || len(data)-i < buildInfoSize {
			return "", "", errNotGoExe
		}
		if i%buildInfoAlign == 0 && len(data)-i >= buildInfoSize {
			data = data[i:]
			break
		}
		data = data[(i+buildInfoAlign-1)&^(buildInfoAlign-1):]
	}

	// Decode the blob.
	// The first 14 bytes are buildInfoMagic.
	// The next two bytes indicate pointer size in bytes (4 or 8) and endianness
	// (0 for little, 1 for big).
	// Two virtual addresses to Go strings follow that: runtime.buildVersion,
	// and runtime.modinfo.
	// On 32-bit platforms, the last 8 bytes are unused.
	// If the endianness has the 2 bit set, then the pointers are zero
	// and the 32-byte header is followed by varint-prefixed string data
	// for the two string values we care about.
	ptrSize := int(data[14])
	if data[15]&2 != 0 {
		vers, data = decodeString(data[32:])
		mod, data = decodeString(data)
	} else {
		bigEndian := data[15] != 0
		var bo binary.ByteOrder
		if bigEndian {
			bo = binary.BigEndian
		} else {
			bo = binary.LittleEndian
		}
		var readPtr func([]byte) uint64
		if ptrSize == 4 {
			readPtr = func(b []byte) uint64 { return uint64(bo.Uint32(b)) }
		} else if ptrSize == 8 {
			readPtr = bo.Uint64
		} else {
			return "", "", errNotGoExe
		}
		vers = readString(x, ptrSize, readPtr, readPtr(data[16:]))
		mod = readString(x, ptrSize, readPtr, readPtr(data[16+ptrSize:]))
	}
	if vers == "" {
		return "", "", errNotGoExe
	}
	if len(mod) >= 33 && mod[len(mod)-17] == '\n' {
		// Strip module framing: sentinel strings delimiting the module info.
		// These are cmd/go/internal/modload.infoStart and infoEnd.
		mod = mod[16 : len(mod)-16]
	} else {
		mod = ""
	}

	return vers, mod, nil
}

func decodeString(data []byte) (s string, rest []byte) {
	u, n := binary.Uvarint(data)
	if n <= 0 || u >= uint64(len(data)-n) {
		return "", nil
	}
	return string(data[n : uint64(n)+u]), data[uint64(n)+u:]
}

// readString returns the string at address addr in the executable x.
func readString(x exe, ptrSize int, readPtr func([]byte) uint64, addr uint64) string {
	hdr, err := x.ReadData(addr, uint64(2*ptrSize))
	if err != nil || len(hdr) < 2*ptrSize {
		return ""
	}
	dataAddr := readPtr(hdr)
	dataLen := readPtr(hdr[ptrSize:])
	data, err := x.ReadData(dataAddr, dataLen)
	if err != nil || uint64(len(data)) < dataLen {
		return ""
	}
	return string(data)
}

// elfExe is the ELF implementation of the exe interface.
type elfExe struct {
	f *elf.File
}

func (x *elfExe) ReadData(addr, size uint64) ([]byte, error) {
	for _, prog := range x.f.Progs {
		if prog.Vaddr <= addr && addr <= prog.Vaddr+prog.Filesz-1 {
			n := prog.Vaddr + prog.Filesz - addr
			if n > size {
				n = size
			}
			data := make([]byte, n)
			_, err := prog.ReadAt(data, int64(addr-prog.Vaddr))
			if err != nil {
				return nil, err
			}
			return data, nil
		}
	}
	return nil, errUnrecognizedFormat
}

func (x *elfExe) DataStart() uint64 {
	for _, s := range x.f.Sections {
		if s.Name == ".go.buildinfo" {
			return s.Addr
		}
	}

	for _, p := range x.f.Progs {
		flags := elf.ProgFlag(elf.PF_R | elf.PF_W)
		if p.Type == elf.PT_LOAD && (p.Flags&flags) == flags {
			return p.Vaddr
		}
	}
	return 0
}

// peExe is the PE (Windows Portable Executable) implementation of the exe interface.
type peExe struct {
	f *pe.File
}

func (x *peExe) imageBase() uint64 {
	switch oh := x.f.OptionalHeader.(type) {
	case *pe.OptionalHeader32:
		return uint64(oh.ImageBase)
	case *pe.OptionalHeader64:
		return oh.ImageBase
	}
	return 0
}

func (x *peExe) ReadData(addr, size uint64) ([]byte, error) {
	addr -= x.imageBase()
	for _, sect := range x.f.Sections {
		if uint64(sect.VirtualAddress) <= addr && addr <= uint64(sect.VirtualAddress+sect.Size-1) {
			n := uint64(sect.VirtualAddress+sect.Size) - addr
			if n > size {
				n = size
			}
			data := make([]byte, n)
			_, err := sect.ReadAt(data, int64(addr-uint64(sect.VirtualAddress)))
			if err != nil {
				return nil, errUnrecognizedFormat
			}
			return data, nil
		}
	}
	return nil, errUnrecognizedFormat
}

func (x *peExe) DataStart() uint64 {
	// Assume data is first writable section.
	const (
		IMAGE_SCN_CNT_CODE               = 0x00000020
		IMAGE_SCN_CNT_INITIALIZED_DATA   = 0x00000040
		IMAGE_SCN_CNT_UNINITIALIZED_DATA = 0x00000080
		IMAGE_SCN_MEM_EXECUTE            = 0x20000000
		IMAGE_SCN_MEM_READ               = 0x40000000
		IMAGE_SCN_MEM_WRITE              = 0x80000000
		IMAGE_SCN_MEM_DISCARDABLE        = 0x2000000
		IMAGE_SCN_LNK_NRELOC_OVFL        = 0x1000000
		IMAGE_SCN_ALIGN_32BYTES          = 0x600000
	)
	for _, sect := range x.f.Sections {
		flags := uint32(IMAGE_SCN_CNT_INITIALIZED_DATA | IMAGE_SCN_MEM_READ | IMAGE_SCN_MEM_WRITE)
		if sect.VirtualAddress != 0 && (sect.Characteristics&flags) == flags {
			return uint64(sect.VirtualAddress) + x.imageBase()
		}
	}
	return 0
}

// machoExe is the Mach-O (Apple macOS/iOS) implementation of the exe interface.
type machoExe struct {
	f *macho.File
}

func (x *machoExe) ReadData(addr, size uint64) ([]byte, error) {
	for _, load := range x.f.Loads {
		seg, ok := load.(*macho.Segment)
		if !ok {
			continue
		}
		if seg.Addr <= addr && addr <= seg.Addr+seg.Filesz-1 {
			if seg.Name == "__PAGEZERO" {
				continue
			}
			n := seg.Addr + seg.Filesz - addr
			if n > size {
				n = size
			}
			data := make([]byte, n)
			_, err := seg.ReadAt(data, int64(addr-seg.Addr))
			if err != nil {
				return nil, err
			}
			return data, nil
		}
	}
	return nil, errUnrecognizedFormat
}

func (x *machoExe) DataStart() uint64 {
	// Look for section named "__go_buildinfo".
	for _, sec := range x.f.Sections {
		if sec.Name == "__go_buildinfo" {
			return sec.Addr
		}
	}
	// Try the first non-empty writable segment.
	const RW = 3
	for _, load := range x.f.Loads {
		seg, ok := load.(*macho.Segment)
		if ok && seg.Addr != 0 && seg.Filesz != 0 && seg.Prot == RW && seg.Maxprot == RW {
			return seg.Addr
		}
	}
	return 0
}

// xcoffExe is the XCOFF (AIX eXtended COFF) implementation of the exe interface.
type xcoffExe struct {
	f *xcoff.File
}

func (x *xcoffExe) ReadData(addr, size uint64) ([]byte, error) {
	for _, sect := range x.f.Sections {
		if uint64(sect.VirtualAddress) <= addr && addr <= uint64(sect.VirtualAddress+sect.Size-1) {
			n := uint64(sect.VirtualAddress+sect.Size) - addr
			if n > size {
				n = size
			}
			data := make([]byte, n)
			_, err := sect.ReadAt(data, int64(addr-uint64(sect.VirtualAddress)))
			if err != nil {
				return nil, err
			}
			return data, nil
		}
	}
	return nil, fmt.Errorf("address not mapped")
}

func (x *xcoffExe) DataStart() uint64 {
	if s := x.f.SectionByType(xcoff.STYP_DATA); s != nil {
		return s.VirtualAddress
	}
	return 0
}
