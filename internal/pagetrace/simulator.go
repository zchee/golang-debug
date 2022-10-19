// Copyright 2022 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pagetrace

import (
	"fmt"
	"time"
)

// Simulator simulates the address space of an application.
type Simulator struct {
	clock time.Duration
	state State
}

// SetState sets up the simulator's state at a particular snapshot.
//
// SetState clones the state before using it.
func (s *Simulator) SetState(state *State) {
	s.state = *(state.Clone())
}

// Validate returns an error if there's something inconsistent
// about the provided event in the context of the current simulation
// state. Useful for detecting errors in the trace and in the trace
// parser. Should be called on an event that's about to be fed into
// the simulator.
func (s *Simulator) Validate(e Event) error {
	if e.Kind == EventBad {
		return fmt.Errorf("found bad event")
	}
	if e.Base%pageSize != 0 {
		return fmt.Errorf("base address 0x%x not aligned to page size", e.Base)
	}
	if e.Size%pageSize != 0 {
		return fmt.Errorf("region size 0x%x not aligned to page size", e.Size)
	}
	if e.Time < s.clock {
		return fmt.Errorf("out-of-order event discovered")
	}
	switch e.Kind {
	case EventAllocate:
		if amt := s.state.Allocated(e.Base, e.Size); amt != 0 {
			return fmt.Errorf("double allocation discovered: want 0, got %d", amt)
		}
	case EventFree:
		if amt := s.state.Allocated(e.Base, e.Size); amt != e.Size {
			return fmt.Errorf("double free discovered: want %d, got %d", e.Size, amt)
		}
	}
	return nil
}

// Feed feeds an event into the simulator, moving time forward.
func (s *Simulator) Feed(e Event) {
	s.clock = e.Time
	s.state.update(e)
}

// Snapshot returns the current state of memory.
//
// The returned State must not be modified and must not be observed after
// the next Feed call. To do either, Clone the state first.
func (s *Simulator) Snapshot() *State {
	return &s.state
}

// State is the state of the address space at a point in the simulation.
type State struct {
	minAddr   uint64
	allocBits []byte
	scavBits  []byte
}

// IsAllocated returns true if the memory at addr is allocated by the Go runtime.
func (s *State) IsAllocated(addr uint64) bool {
	if addr < s.MinAddr() || addr >= s.MaxAddr() {
		return false
	}
	off := (addr - s.minAddr) / pageSize
	return s.allocBits[off/8]&(1<<(off%8)) != 0
}

// Allocated returns the amount of memory allocated in the memory region [addr, addr+size).
func (s *State) Allocated(addr, size uint64) uint64 {
	var sum uint64
	start := alignDown(addr, pageSize)
	if start < s.MinAddr() {
		start = s.MinAddr()
	}
	end := alignUp(addr+size, pageSize)
	if end > s.MaxAddr() {
		end = s.MaxAddr()
	}
	for i := start; i < end; i += pageSize {
		if !s.IsAllocated(i) {
			continue
		}
		if i < addr {
			sum += alignUp(addr, pageSize) - i
		} else if i+pageSize > addr+size {
			sum += addr + size - i
		} else {
			sum += pageSize
		}
	}
	return sum
}

// IsScavenged returns true if the memory at addr is free and scavenged.
func (s *State) IsScavenged(addr uint64) bool {
	if addr < s.MinAddr() || addr >= s.MaxAddr() {
		return true
	}
	off := (addr - s.minAddr) / pageSize
	return s.scavBits[off/8]&(1<<(off%8)) != 0
}

// Scavenged returns the amount of memory scavenged in the memory region [addr, addr+size).
func (s *State) Scavenged(addr, size uint64) uint64 {
	var sum uint64
	start := alignDown(addr, pageSize)
	if start < s.MinAddr() {
		start = s.MinAddr()
	}
	end := alignUp(addr+size, pageSize)
	if end > s.MaxAddr() {
		end = s.MaxAddr()
	}
	for i := start; i < end; i += pageSize {
		if !s.IsScavenged(i) {
			continue
		}
		if i < addr {
			sum += alignUp(addr, pageSize) - i
		} else if i+pageSize > addr+size {
			sum += addr + size - i
		} else {
			sum += pageSize
		}
	}
	return sum
}

// Clone makes a copy of the State.
func (s *State) Clone() *State {
	s2 := *s
	s2.allocBits = make([]byte, len(s.allocBits))
	s2.scavBits = make([]byte, len(s.scavBits))
	copy(s2.allocBits, s.allocBits)
	copy(s2.scavBits, s.scavBits)
	return &s2
}

// Size returns the size of the memory region tracked. Note that this may be
// larger than the peak memory size described by events used to construct this
// State.
func (s *State) Size() uint64 {
	return uint64(len(s.allocBits)) * 8 * pageSize
}

// MinAddr returns the minimum address tracked by the state. Note this may be
// lower than the actual minimum address of any event used to construct this State.
func (s *State) MinAddr() uint64 {
	return s.minAddr
}

// MaxAddr returns the maximum address encountered so far. Note that this may be
// larger than the actual maximum address of any event used to construct this State.
func (s *State) MaxAddr() uint64 {
	return s.minAddr + s.Size()
}

// update updates the State based on an event.
func (s *State) update(e Event) {
	minAddr, maxAddr := e.Base, e.Base+e.Size
	if s.allocBits == nil {
		s.minAddr = minAddr
		s.allocBits = make([]byte, bitmapSize(minAddr, maxAddr))
		s.scavBits = make([]byte, bitmapSize(minAddr, maxAddr))
	} else {
		if maxAddr > s.MaxAddr() {
			newSize := bitmapSize(s.MinAddr(), maxAddr)
			if len(s.allocBits) < newSize {
				s.allocBits = append(s.allocBits, make([]byte, newSize-len(s.allocBits))...)
			}
			if len(s.scavBits) < newSize {
				s.scavBits = append(s.scavBits, make([]byte, newSize-len(s.scavBits))...)
			}
		}
		if minAddr < s.MinAddr() {
			s.minAddr = minAddr
			newSize := bitmapSize(s.MinAddr(), s.MaxAddr())
			if len(s.allocBits) < newSize {
				s.allocBits = append(make([]byte, newSize-len(s.allocBits)), s.allocBits...)
			}
			if len(s.scavBits) < newSize {
				s.scavBits = append(make([]byte, newSize-len(s.scavBits)), s.scavBits...)
			}
		}
	}
	off := (e.Base - s.minAddr) / pageSize
	for i := off; i < (e.Base+e.Size-s.minAddr)/pageSize; i++ {
		//println(len(s.allocBits), i/8, (e.Base+e.Size-s.minAddr)/pageSize, unsafe.Pointer(uintptr(s.MaxAddr())), unsafe.Pointer(uintptr(e.Base+e.Size)))
		switch e.Kind {
		case EventAllocate:
			s.allocBits[i/8] |= 1 << (i % 8)
			s.scavBits[i/8] &^= 1 << (i % 8)
		case EventFree:
			s.allocBits[i/8] &^= 1 << (i % 8)
		case EventScavenge:
			s.scavBits[i/8] |= 1 << (i % 8)
		default:
			// N.B. We don't do validation here.
			return
		}
	}
}

func alignUp(x, align uint64) uint64 {
	return (x + align - 1) &^ (align - 1)
}

func alignDown(x, align uint64) uint64 {
	return x &^ (align - 1)
}

func bitmapSize(minAddr, maxAddr uint64) int {
	return int(alignUp(alignUp(maxAddr-minAddr, pageSize)/pageSize, 8) / 8)
}
