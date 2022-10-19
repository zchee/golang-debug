// Copyright 2022 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pagetrace

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// Trace represents a slice of a page trace in time.
type Trace struct {
	r            io.ReaderAt
	blocks       [][]interval
	minTraceTime int64
	startTime    int64
	endTime      int64
	minAddr      uint64
	maxAddr      uint64
}

// NewTrace creates a new Trace from an encoded trace.
//
// The returned Trace represents the full trace from beginning to end.
func NewTrace(r io.ReaderAt) (*Trace, error) {
	// Parse the trace once through to obtain some useful information about it.
	var (
		// Reader state.
		buf    [32 << 10]byte
		cursor int64

		expectNpagesTrailer bool // Indicates whether the next 8 bytes is a large npages trailer.
		wantTime            bool // Set after a pid event. Indicates that we want a sync event.

		// startTime is the minimum timestamp of any event in the trace.
		startTime int64

		// endTime is the maximum timestamp of any event in the trace.
		endTime int64

		// syncTime is the timestamp of the last sync event seen.
		syncTime int64

		// minAddr is the minumum address of any memory region encountered in an event.
		minAddr uint64

		// maxAddr is the maximum address of any memory region encountered in an event.
		maxAddr uint64

		// npagesTrailerBaseAddr is only valid if expectNpagesTrailer is true. It's the base
		// address of the event for which we're about to read the npages trailer.
		npagesTrailerBaseAddr uint64

		// cur represents the interval of the current block of events for the current P.
		//
		// Written to t.blocks when we discover a new pid event.
		cur *interval = new(interval)

		// t is the Trace we're constructing.
		t *Trace = &Trace{r: r}
	)
	for {
		n, err := t.r.ReadAt(buf[:], cursor)
		if n%8 != 0 {
			return nil, fmt.Errorf("malformed trace: not a multiple of 8 in size")
		}
		for j := 0; j < n; j += 8 {
			if expectNpagesTrailer {
				npages := binary.LittleEndian.Uint64(buf[j : j+8])
				if max := npagesTrailerBaseAddr + npages*pageSize; maxAddr == 0 || max > maxAddr {
					maxAddr = max
				}
				expectNpagesTrailer = false
				continue
			}
			e := eventHeader(binary.LittleEndian.Uint64(buf[j : j+8]))
			var curTime int64
			if e.kind() != pid {
				if e.kind() == sync {
					curTime = e.timestamp()
					syncTime = e.timestamp()
				} else {
					if e.large() {
						expectNpagesTrailer = true
					}
					min := e.base()
					if minAddr == 0 || min < minAddr {
						minAddr = min
					}
					if e.large() {
						npagesTrailerBaseAddr = min
					} else {
						max := min + e.npagesSmall()*pageSize
						if maxAddr == 0 || max > maxAddr {
							maxAddr = max
						}
					}
					curTime = syncTime + e.timestampDelta()
				}
				if curTime > endTime {
					endTime = curTime
				}
			}
			if wantTime {
				if e.kind() != sync {
					return nil, fmt.Errorf("expected sync event immeditately following pid event")
				}
				t := e.timestamp()
				if startTime == 0 || t < startTime {
					startTime = t
				}
				cur.startTime = t
				wantTime = false
				continue
			}
			if e.kind() != pid {
				continue
			}

			// Construct blocks.
			cur.end = cursor + int64(j)
			cur.endTime = curTime
			idx := pidIndex(e.pid())
			np := idx + 1
			if len(t.blocks) < np {
				t.blocks = append(t.blocks, make([][]interval, np-len(t.blocks))...)
			}
			t.blocks[idx] = append(t.blocks[idx], interval{})
			cur = &t.blocks[idx][len(t.blocks[idx])-1]
			cur.start = cursor + int64(j)
			wantTime = true
		}
		cursor += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	cur.end = cursor
	t.minTraceTime = startTime
	t.startTime = startTime
	t.endTime = endTime
	t.minAddr = minAddr
	t.maxAddr = maxAddr
	return t, nil
}

// Duration returns the real monotonic wall-time duration during which
// the trace was taken.
func (t *Trace) Duration() time.Duration {
	return time.Duration(t.endTime - t.startTime)
}

// Clone makes a copy of the trace.
func (t *Trace) Clone() *Trace {
	t2 := new(Trace)
	t2.r = t.r
	t2.startTime = t.startTime
	t2.endTime = t.endTime
	t2.minTraceTime = t.minTraceTime
	t2.blocks = make([][]interval, len(t.blocks))
	for i := range t.blocks {
		t2.blocks[i] = make([]interval, len(t.blocks[i]))
		copy(t2.blocks[i], t.blocks[i])
	}
	return t2
}

// TimeStart returns the start time of the trace as a duration
// in nanoseconds since the true start of the page trace.
//
// A Trace returned by NewTrace will return 0, and a sliced
// Trace will return the start time of the slice.
func (t *Trace) TimeStart() time.Duration {
	return time.Duration(t.startTime - t.minTraceTime)
}

// TimeEnd returns the start time of the trace as a duration
// in nanoseconds since the true start of the page trace.
//
// A Trace returned by NewTrace will return the trace duration,
// and a sliced Trace will return the end time of the slice.
func (t *Trace) TimeEnd() time.Duration {
	return time.Duration(t.endTime - t.minTraceTime)
}

// MinAddr returns the minimum address of any address in the trace.
//
// Note this is the minimum address for the entire trace, not the slice.
func (t *Trace) MinAddr() uint64 {
	return t.minAddr
}

// MaxAddr returns the maximum address of any address in the trace.
//
// Note this is the maximum address for the entire trace, not the slice.
func (t *Trace) MaxAddr() uint64 {
	return t.maxAddr
}

// Slice creates a slice of Trace from time s to time e.
//
// Both s and e must be defined not relative to this Trace,
// but to the true Trace. This is because a Trace slice isn't
// meaningful without the context of the full trace.
//
// s and e will be clamped to the current bounds of Trace before
// slicing.
func (t *Trace) Slice(s, e time.Duration) *Trace {
	start := t.minTraceTime + int64(s)
	end := t.minTraceTime + int64(e)
	t2 := new(Trace)
	t2.r = t.r
	t2.minAddr = t.minAddr
	t2.maxAddr = t.maxAddr
	t2.minTraceTime = t.minTraceTime
	t2.blocks = make([][]interval, len(t.blocks))
	if end > t.endTime {
		end = t.endTime
	}
	if start < t.startTime {
		start = t.startTime
	}
	if end <= start {
		t2.startTime = t.minTraceTime
		t2.endTime = t.minTraceTime
		return t2
	}
	t2.startTime = start
	t2.endTime = end
	for i := range t.blocks {
		for _, iv := range t.blocks[i] {
			if iv.endTime < start || iv.startTime > end {
				continue
			}
			t2.blocks[i] = append(t2.blocks[i], iv)
		}
	}
	return t2
}
