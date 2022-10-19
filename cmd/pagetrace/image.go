package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"time"

	"golang.org/x/debug/internal/pagetrace"
)

type imageCmd struct {
}

func (c *imageCmd) Name() string {
	return "image"
}

func (c *imageCmd) Description() string {
	return "dumps an image visualizing the state of memory in the trace over time"
}

func (c *imageCmd) Run(args []string) error {
	fs := subcommandFlags(c)
	timeGranule := fs.Duration("time-granule", 0, "size of each time granule")
	memGranule := fs.Uint64("mem-granule", 0, "size of each memory granule in bytes")
	memGranuleAlign := fs.Uint64("mem-granule-align", 1, "address alignment of each granule")
	outputFile := fs.String("output", "", "where to write the png image")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("print expected one argument: a trace")
	}
	if *timeGranule == 0 {
		return fmt.Errorf("must specify -time-granule")
	}
	if *memGranule == 0 {
		return fmt.Errorf("must specify -mem-granule")
	}
	if *outputFile == "" {
		return fmt.Errorf("must specify -output")
	}
	traceFile := fs.Arg(0)
	f, err := os.Open(traceFile)
	if err != nil {
		return err
	}
	defer f.Close()
	t, err := pagetrace.NewTrace(f)
	if err != nil {
		return err
	}
	snaps, _, err := takeSnapshots(t, *timeGranule, nil)
	if err != nil {
		return err
	}

	memChunk := *memGranule
	memAlign := *memGranuleAlign

	minAddr, maxAddr := t.MinAddr(), t.MaxAddr()

	minAddr = alignDown(minAddr, memAlign)
	maxAddr = alignUp(maxAddr, memAlign)

	img := makeImage(snaps, minAddr, maxAddr, memChunk)

	outf, err := os.Create(*outputFile)
	if err != nil {
		return err
	}
	defer outf.Close()
	return png.Encode(outf, img)
}

func makeImage(snaps []*pagetrace.State, minAddr, maxAddr, memChunk uint64) *image.RGBA {
	// Align up maxAddr to memChunk. It makes the output size much more predictable.
	maxAddr = minAddr + (((maxAddr-minAddr)+memChunk-1)/memChunk)*memChunk
	width := len(snaps)
	height := int((maxAddr - minAddr) / memChunk)
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x, snap := range snaps {
		for i, y := minAddr, height-1; i < maxAddr; i, y = i+memChunk, y-1 {
			if snap == nil {
				img.SetRGBA(x, y, color.RGBA{0, 0, 0, 255})
				continue
			}
			occupancy := float64(snap.Allocated(i, memChunk)) / float64(memChunk)
			if occupancy == 0 {
				img.SetRGBA(x, y, color.RGBA{0, 0, 0, 255})
			} else {
				img.SetRGBA(x, y, viridis.Map(occupancy).(color.RGBA))
			}
		}
	}
	return img
}

func alignDown(v, align uint64) uint64 {
	return (v / align) * align
}

func alignUp(v, align uint64) uint64 {
	return ((v + align - 1) / align) * align
}

func takeSnapshots(t *pagetrace.Trace, timeGranule time.Duration, start *pagetrace.State) ([]*pagetrace.State, *pagetrace.State, error) {
	parser := pagetrace.NewParser(t)
	var snaps []*pagetrace.State
	var sim pagetrace.Simulator
	if start != nil {
		sim.SetState(start)
	}
	snaps = append(snaps, start)
	now := t.TimeStart()
	lastSnapTime := now
	for {
		for now-lastSnapTime > timeGranule {
			// Loop because we may need to add time in between events.
			snaps = append(snaps, sim.Snapshot().Clone())
			lastSnapTime += timeGranule
		}
		e, err := parser.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		sim.Feed(e)
		now = e.Time
	}
	last := sim.Snapshot().Clone()
	for t.TimeStart()+time.Duration(len(snaps))*timeGranule < t.TimeEnd() {
		snaps = append(snaps, last)
	}
	return snaps, last, nil
}
