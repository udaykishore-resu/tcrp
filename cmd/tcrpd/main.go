// Command tcrpd is the Tokenized Collateral Risk Platform.
//
// One binary, several subcommands, one object graph. `serve` runs the HTTP API
// and the governance dashboard; `demo` narrates the whole platform in front of
// you with real numbers; `ask` runs one governed agent turn from the terminal;
// `eval` runs the benchmark suite; `conform` runs the conformance gate. Every
// one of them builds the platform through internal/platform, so a control
// cannot be present in one entry point and absent from another.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// Build information, set by the linker.
var (
	version = "dev"
	commit  = "none"
	builtAt = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, os.Args[2:])
	case "demo":
		err = demo(ctx, os.Args[2:])
	case "ask":
		err = ask(ctx, os.Args[2:])
	case "eval":
		err = runEval(ctx, os.Args[2:])
	case "conform":
		err = runConform(ctx, os.Args[2:])
	case "version":
		fmt.Printf("tcrpd %s (commit %s, built %s)\n", version, commit, builtAt)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "tcrpd: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "tcrpd: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `tcrpd — Tokenized Collateral Risk Platform

  tcrpd demo               narrate the whole platform, end to end, with real numbers
  tcrpd serve              run the HTTP API and the governance dashboard
  tcrpd ask "<question>"   run one governed agent turn
  tcrpd eval               run the evaluation suite against the runtime
  tcrpd conform            run the conformance gate and write an evidence bundle
  tcrpd version            print build information

Run any subcommand with -h for its flags.
`)
}

// flagsFor builds a flag set that prints to stderr and does not exit on error,
// so a bad flag produces the same failure path as any other error.
func flagsFor(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}
