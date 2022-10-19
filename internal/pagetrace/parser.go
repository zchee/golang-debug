// Copyright 2022 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pagetrace

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"
)

// Parser represents a parser for a page trace.
type Parser struct {
	trace  *Trace
	clocks []int64
	events []event
}

type interval struct {
	start, end         int64
	startTime, endTime int64
}

// NewParser creates a parser for the provided trace, which it clones.
func NewParser(t *Trace) *Parser {
	return &Parser{trace: t.Clone()}
}

// pidIndex converts a P ID into something that can be used as an array index.
func pidIndex(pid int32) int {
	// Add 1 so that pid -1 is at index 0.
	return int(pid + 1)
}

// pidFromIndex converts a P index back into an ID.
func pidFromIndex(pidx int) int32 {
	return int32(pidx - 1)
}

// read8 reads the next 8 bytes in the trace from the event stream
// for the P with ID pidFromIndex(pidx).
func (p *Parser) read8(pidx int) (uint64, error) {
	if len(p.trace.blocks[pidx]) == 0 {
		return 0, nil
	}
	i := p.trace.blocks[pidx][0]
	var u [8]byte
	_, err := p.trace.r.ReadAt(u[:], i.start)
	if err != nil {
		return 0, fmt.Errorf("reading for P %d: %v", pidx, err)
	}
	i.start += 8
	if i.start == i.end {
		p.trace.blocks[pidx] = p.trace.blocks[pidx][1:]
	} else {
		p.trace.blocks[pidx][0] = i
	}
	return binary.LittleEndian.Uint64(u[:]), nil
}

// refreshEvent gets the next event for the provided P.
//
// pidx is the P index, not the actual P ID. See pidIndex.
func (p *Parser) refreshEvent(pidx int) (event, error) {
	for {
		u, err := p.read8(pidx)
		if err != nil {
			return event{}, err
		}
		if u == 0 {
			return event{}, nil
		}
		e := event{eventHeader: eventHeader(u)}
		if k := e.kind(); k == sync {
			p.clocks[pidx] = e.timestamp()
			continue
		} else if k == pid {
			if pidIndex(e.pid()) != pidx {
				return e, fmt.Errorf("encountered pid event for P %d, but want P %d", e.pid(), pidFromIndex(pidx))
			}
			continue
		}
		if e.large() {
			e.npages, err = p.read8(pidx)
			if err != nil {
				return e, fmt.Errorf("reading npages: %v", err)
			}
		} else {
			e.npages = e.npagesSmall()
		}
		return e, nil
	}
}

// event represents a complete encoded event. It contains both the 8-byte
// event itself and any trailers that event may have (e.g. 8 bytes of npages).
type event struct {
	eventHeader
	npages uint64
}

// makeEvent creates an Event from an event.
func (e event) makeEvent(p int32, timestamp, startTimestamp int64) Event {
	switch e.kind() {
	case sync, pid:
		panic("cannot make event for kind")
	}
	return Event{
		Kind: Kind(e.kindNoLarge()),
		P:    p,
		Time: time.Duration(timestamp - startTimestamp),
		Base: e.base(),
		Size: pageSize * e.npages,
	}
}

const ()

// eventKind is the combined event type and "large" bit.
//
// See src/runtime/pagetrace_on.go in the upstream Go repository.
type eventKind uint8

const (
	// Constants from the runtime describing various things, such
	// as the size of pages in bytes and various aspects of the trace
	// encoding. Must line up with src/runtime/pagetrace_on.go in the
	// upstream Go repository.
	sync eventKind = iota
	alloc
	free
	scav
	pid
	allocLarge
	freeLarge
	scavLarge

	kindBits = 3
	kindMask = (1 << kindBits) - 1

	pageShift     = 13
	pageSize      = 1 << pageShift
	timeLostBits  = 7
	timeDeltaBits = 16
	heapAddrBits  = 48
)

// eventHeader represents an encoded 8-byte event header. For most events,
// this is the entire event.
type eventHeader uint64

// kind returns the eventKind for the partial event.
func (e eventHeader) kind() eventKind {
	return eventKind(e & kindMask)
}

// large returns whether the event refers to a large region of memory.
//
// Panics if kind is sync or pid.
func (e eventHeader) large() bool {
	if k := e.kind(); k == pid || k == sync {
		panic("large called on bad event")
	}
	return (e & (1 << 2)) != 0
}

// kindNoLarge returns the eventKind, but ignoring the "large" bit.
//
// That is, the only possible results are sync, alloc, free, or scav.
func (e eventHeader) kindNoLarge() eventKind {
	return eventKind(e & (kindMask >> 1))
}

// pid returns the pid stored in the event. Panics if kind() != pid.
func (e eventHeader) pid() int32 {
	if e.kind() != pid {
		panic("event kind must be pid to get pid")
	}
	return int32(int64(e) >> kindBits)
}

// timestamp returns a full timestamp. Panics if kind() != sync.
func (e eventHeader) timestamp() int64 {
	if e.kind() != sync {
		panic("event kind must be sync to get timestamp")
	}
	return int64(e&^kindMask) << (timeLostBits - kindBits)
}

// timestampDelta returns a timestamp delta. Panics if kind() == sync or pid.
func (e eventHeader) timestampDelta() int64 {
	if k := e.kind(); k == pid || k == sync {
		panic("timestampDelta called on bad event")
	}
	return int64(e >> ((64 - timeDeltaBits) - timeLostBits))
}

// base returns the base address for the event. Panics if kind() == sync or pid.
func (e eventHeader) base() uint64 {
	if k := e.kind(); k == pid || k == sync {
		panic("timestampDelta called on bad event")
	}
	return uint64(e) &^ (pageSize - 1) & ((1 << heapAddrBits) - 1)
}

// npagesSmall returns the size of the memory region referred to by the event in pages.
//
// Panics if kind() == sync or pid, or if large() is true.
func (e eventHeader) npagesSmall() uint64 {
	if k := e.kind(); k == pid || k == sync || e.large() {
		panic("npagesSmall called on bad event")
	}
	return (uint64(e) >> kindBits) & ((1 << 10) - 1)
}

// Kind is the event type.
type Kind uint8

const (
	EventBad      Kind = iota
	EventAllocate      // Represents an allocation of pages.
	EventFree          // Represents pages being freed.
	EventScavenge      // Represents pages being scavenged. Allocated pages are unscavenged.
)

// String returns a string representation of the event type.
func (k Kind) String() string {
	switch k {
	case EventBad:
		return "ERROR"
	case EventAllocate:
		return "Alloc"
	case EventFree:
		return "Free"
	case EventScavenge:
		return "Scav"
	}
	panic("unsupported kind")
}

// Event represents a single complete event in the page trace.
type Event struct {
	Kind Kind

	// P is the ID of the P this event occurred on. -1 if the event happened without a P.
	P int32

	// Time is the timestamp of event in nanoseconds since the start of the trace.
	Time time.Duration

	// Base is the base address of the memory region that this event happened to.
	Base uint64

	// Size is the size of the memory region in bytes that this event happened to.
	Size uint64
}

// Next returns the next event in the parse stream.
//
// Returns io.EOF at the end of the stream.
func (p *Parser) Next() (Event, error) {
top:
	if len(p.clocks) == 0 {
		// Initialize.
		var err error
		p.clocks = make([]int64, len(p.trace.blocks))
		p.events = make([]event, len(p.trace.blocks))
		for i := range p.trace.blocks {
			p.events[i], err = p.refreshEvent(i)
			if err != nil {
				return Event{}, err
			}
		}
	}
	var nextpidx int
	var nexte event
	minTimestamp := int64(math.MaxInt64)
	for i, e := range p.events {
		if e.eventHeader == eventHeader(0) {
			continue
		}
		timestamp := p.clocks[i] + e.timestampDelta()
		if minTimestamp > timestamp {
			minTimestamp = timestamp
			nextpidx = i
			nexte = e
		}
	}
	if minTimestamp == math.MaxInt64 {
		return Event{}, io.EOF
	}
	e := nexte.makeEvent(pidFromIndex(nextpidx), minTimestamp, p.trace.minTraceTime)
	if e.Time > p.trace.TimeEnd() {
		return Event{}, io.EOF
	}
	var err error
	p.events[nextpidx], err = p.refreshEvent(nextpidx)
	if err != nil {
		return Event{}, err
	}
	if e.Time < p.trace.TimeStart() {
		goto top
	}
	return e, nil
}

// Rest returns the as-yet unparsed part of the trace.
func (p *Parser) Rest() *Trace {
	t := p.trace.Clone()
	minTimestamp := int64(math.MaxInt64)
	for i, e := range p.events {
		if e.eventHeader == eventHeader(0) {
			continue
		}
		timestamp := p.clocks[i] + e.timestampDelta()
		if minTimestamp > timestamp {
			minTimestamp = timestamp
		}
	}
	t.startTime = minTimestamp
	return t
}
