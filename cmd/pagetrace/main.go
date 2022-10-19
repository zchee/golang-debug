package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
)

type subcmd struct {
	desc string
	run  func(args []string) error
}

type subcommand interface {
	Name() string
	Description() string
	Run([]string) error
}

var subcmds map[string]subcommand

func register(s subcommand) {
	if subcmds == nil {
		subcmds = make(map[string]subcommand)
	}
	subcmds[s.Name()] = s
}

func run() (bool, error) {
	for i, arg := range os.Args {
		cmd, ok := subcmds[arg]
		if !ok {
			continue
		}
		if err := globalFlags.Parse(os.Args[:i]); err != nil {
			return true, err
		}
		return false, cmd.Run(os.Args[i+1:])
	}
	return true, fmt.Errorf("no command specified")
}

func subcommandFlags(s subcommand) *flag.FlagSet {
	name := s.Name()
	desc := s.Description()
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [global flags] %s [%s flags] <trace>\n", os.Args[0], name, name)
		fmt.Fprintf(os.Stderr, "The %s command %s.\n", name, desc)
		fmt.Fprintf(os.Stderr, "\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "Global flags:\n")
		globalFlags.PrintDefaults()
	}
	return fs
}

var globalFlags *flag.FlagSet

func init() {
	globalFlags = flag.NewFlagSet("global", flag.ExitOnError)
	globalFlags.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [global flags] <command> [command flags]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "Commands:\n")
		var names []string
		for name := range subcmds {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(os.Stderr, "\t%s\t%s\n", name, subcmds[name].Description())
		}
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "Global flags:\n")
		globalFlags.PrintDefaults()
	}
	register(&printCmd{})
	register(&imageCmd{})
	register(&viewCmd{})
}

func main() {
	if usage, err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		if usage {
			globalFlags.Usage()
		}
		os.Exit(1)
	}
}
