package main

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/debug/internal/pagetrace"
)

type printCmd struct {
}

func (p *printCmd) Name() string {
	return "print"
}

func (p *printCmd) Description() string {
	return "dumps every event in the trace to the terminal"
}

func (p *printCmd) Run(args []string) error {
	fs := subcommandFlags(p)
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
	parser := pagetrace.NewParser(t)
	var sim pagetrace.Simulator
	for {
		e, err := parser.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		giga := int64(e.Time) / 1e9
		left := int64(e.Time) % 1e9
		fmt.Fprintf(os.Stdout, "[P %d %d.%09d] %s(0x%x, %d)\n", e.P, giga, left, e.Kind, e.Base, e.Size)
		if err := sim.Validate(e); err != nil {
			fmt.Fprintf(os.Stdout, "error: %v\n", err)
		}
		sim.Feed(e)
	}
	return nil
}
