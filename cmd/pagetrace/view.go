package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/debug/internal/pagetrace"
)

type viewCmd struct {
}

func (c *viewCmd) Name() string {
	return "view"
}

func (c *viewCmd) Description() string {
	return "visualizes the trace in an interactive web environment"
}

func (c *viewCmd) Run(args []string) error {
	fs := subcommandFlags(c)
	host := fs.String("http", "localhost:8080", "host and port combination for the web server")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("print expected one argument: a trace")
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
	tt, err := makeTraceTree(t)
	if err != nil {
		return err
	}
	http.Handle("/", http.FileServer(http.FS(content)))
	http.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != "" {
			log.Print("bad request method ", r.Method)
			http.Error(w, "must be a GET request", http.StatusBadRequest)
			return
		}
		log.Print(r.URL.String())

		ti := &traceInfo{
			Filename:    filepath.Base(traceFile),
			Duration:    int64(tt.span.maxTime),
			MinAddr:     tt.span.minAddr,
			MaxAddr:     tt.span.maxAddr,
			TileSize:    tileSize,
			MinDuration: int64(minDuration),
			MinMemChunk: uint64(minMemChunk),
			MaxDuration: int64(tt.maxTileDuration),
			MaxMemChunk: tt.maxTileMemChunk,
			MagFactor:   magFactor,
			Depth:       uint64(tt.height),
		}
		if err := json.NewEncoder(w).Encode(ti); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})
	http.HandleFunc("/tile", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != "" {
			log.Print("bad request method ", r.Method)
			http.Error(w, "must be a GET request", http.StatusBadRequest)
			return
		}
		log.Print(r.URL.String())
		var t, a, d uint64
		var got int
		var err error
		values := r.URL.Query()
		if v, ok := values["t"]; ok {
			t, err = strconv.ParseUint(v[0], 10, 64)
			if err != nil {
				http.Error(w, fmt.Sprintf("malformed t: %v", err), http.StatusBadRequest)
				return
			}
			got++
		}
		if v, ok := values["a"]; ok {
			a, err = strconv.ParseUint(v[0], 10, 64)
			if err != nil {
				http.Error(w, fmt.Sprintf("malformed a: %v", err), http.StatusBadRequest)
				return
			}
			got++
		}
		if v, ok := values["d"]; ok {
			d, err = strconv.ParseUint(v[0], 10, 64)
			if err != nil {
				http.Error(w, fmt.Sprintf("malformed d: %v", err), http.StatusBadRequest)
				return
			}
			got++
		}
		if got != 3 {
			http.Error(w, "missing t, a, or d query variable", http.StatusBadRequest)
			return
		}
		img, err := tt.getTile(time.Duration(int64(t)), a, int(d))
		if err != nil {
			log.Printf("error retrieving tile: %v", err)
			http.Error(w, "error retrieving tile", http.StatusInternalServerError)
			return
		}
		if img == nil {
			log.Printf("tile not found: %s 0x%x %d", time.Duration(int64(t)), a, d)
			http.Error(w, "tile not found", http.StatusBadRequest)
			return
		}
		if err := png.Encode(w, img); err != nil {
			http.Error(w, "failed to encode image", http.StatusInternalServerError)
			return
		}
	})
	log.Print("Server ready!")
	return http.ListenAndServe(*host, nil)
}

type traceInfo struct {
	Filename    string `json:"filename"`
	Duration    int64  `json:"duration"`
	MinAddr     uint64 `json:"minAddr"`
	MaxAddr     uint64 `json:"maxAddr"`
	TileSize    uint64 `json:"tileSize"`
	MinDuration int64  `json:"minDuration"`
	MinMemChunk uint64 `json:"minMemChunk"`
	MaxDuration int64  `json:"maxDuration"`
	MaxMemChunk uint64 `json:"maxMemChunk"`
	MagFactor   uint64 `json:"magFactor"`
	Depth       uint64 `json:"depth"`
}

type span struct {
	minTime, maxTime time.Duration
	minAddr, maxAddr uint64
}

type traceTree struct {
	t *pagetrace.Trace
	span
	trees           []*snapNode
	height          int
	maxTileDuration time.Duration
	maxTileMemChunk uint64
}

type snapNode struct {
	minTime, maxTime time.Duration
	snaps            [tileSize]*pagetrace.State
	children         [magFactor]*snapNode
	childLocks       [magFactor]sync.Mutex
}

const (
	minDuration     = time.Duration(8192)
	minMemChunk     = uint64(8192)
	tileSize        = 256
	magFactor       = 2
	minTileDuration = minDuration * time.Duration(tileSize)
	minTileMemChunk = uint64(minMemChunk * tileSize)
)

func makeTraceTree(t *pagetrace.Trace) (*traceTree, error) {
	maxDur := t.Duration()
	if maxDur < minTileDuration {
		maxDur = minTileDuration
	}
	minAddr, maxAddr := t.MinAddr(), t.MaxAddr()
	log.Printf("%x, %x", minAddr, maxAddr)
	minAddr = alignDown(minAddr, minTileMemChunk)
	maxAddr = alignUp(maxAddr, minTileMemChunk)
	if maxAddr-minAddr < minTileMemChunk {
		maxAddr = minAddr + minTileMemChunk
	}
	memSize := maxAddr - minAddr
	height := 1
	maxTileDuration := minTileDuration
	maxTileMemChunk := minTileMemChunk
	for maxTileDuration < maxDur && maxTileMemChunk < memSize {
		maxTileDuration *= magFactor
		maxTileMemChunk *= magFactor
		height++
	}
	maxDur = time.Duration(int64(alignUp(uint64(maxDur), uint64(maxTileDuration))))
	maxAddr = minAddr + alignUp(maxAddr-minAddr, maxTileMemChunk)
	trees := make([]*snapNode, maxDur/maxTileDuration)
	var last *pagetrace.State
	for i := range trees {
		start := time.Duration(i) * maxTileDuration
		end := start + maxTileDuration
		var err error
		trees[i], last, err = snapNodeRoot(t.Slice(start, end), last)
		if err != nil {
			return nil, err
		}
	}
	return &traceTree{
		t:               t,
		span:            span{0, maxDur, minAddr, maxAddr},
		trees:           trees,
		height:          height,
		maxTileDuration: maxTileDuration,
		maxTileMemChunk: maxTileMemChunk,
	}, nil
}

func snapNodeRoot(t *pagetrace.Trace, s *pagetrace.State) (*snapNode, *pagetrace.State, error) {
	tileDuration := t.Duration() / tileSize
	log.Print("make node ", t.TimeStart(), t.TimeEnd(), tileDuration)
	snaps, last, err := takeSnapshots(t, tileDuration, s)
	if err != nil {
		return nil, nil, err
	}
	n := &snapNode{
		minTime: t.TimeStart(),
		maxTime: t.TimeEnd(),
	}
	copy(n.snaps[:], snaps)
	log.Print("finish make node ", t.TimeStart(), t.TimeEnd(), tileDuration)
	return n, last, nil
}

func (tt *traceTree) getTile(tileTime time.Duration, tileAddr uint64, d int) (*image.RGBA, error) {
	s := tt.span
	if tileTime < s.minTime || tileTime >= s.maxTime {
		return nil, nil
	}
	if tileAddr < s.minAddr || tileAddr >= s.maxAddr {
		return nil, nil
	}

	s.minAddr = s.minAddr + (tileAddr-s.minAddr)/tt.maxTileMemChunk*tt.maxTileMemChunk
	s.maxAddr = s.minAddr + tt.maxTileMemChunk

	s.minTime = s.minTime + (tileTime-s.minTime)/tt.maxTileDuration*tt.maxTileDuration
	s.maxTime = s.minTime + tt.maxTileDuration

	node := tt.trees[(s.minTime-tt.span.minTime)/tt.maxTileDuration]

	depth := 0
	for depth < tt.height {
		if d == depth && s.minTime == tileTime && s.minAddr == tileAddr {
			return makeImage(node.snaps[:], s.minAddr, s.maxAddr, (s.maxAddr-s.minAddr)/tileSize), nil
		}
		// Go one level deeper.

		nextMemChunk := (s.maxAddr - s.minAddr) / magFactor
		s.minAddr = s.minAddr + (tileAddr-s.minAddr)/nextMemChunk*nextMemChunk
		s.maxAddr = s.minAddr + nextMemChunk

		nextDuration := (s.maxTime - s.minTime) / magFactor
		s.minTime = s.minTime + (tileTime-s.minTime)/nextDuration*nextDuration
		s.maxTime = s.minTime + nextDuration

		childIdx := (s.minTime - node.minTime) / nextDuration
		childSnapIdx := childIdx * tileSize / magFactor
		node.childLocks[childIdx].Lock()
		if node.children[childIdx] == nil {
			// Create the child if it doesn't exist.
			//
			// TODO(mknyszek): There's potential to save a lot of memory
			// since about half of the snapshots we'll generate here are
			// actually available in the parent.
			child, _, err := snapNodeRoot(tt.t.Slice(s.minTime, s.maxTime), node.snaps[childSnapIdx])
			if err != nil {
				node.childLocks[childIdx].Unlock()
				return nil, err
			}
			node.children[childIdx] = child
		}
		tmp := node.children[childIdx]
		node.childLocks[childIdx].Unlock()
		node = tmp
		depth++
	}
	return nil, nil
}

//go:embed index.html
var content embed.FS
