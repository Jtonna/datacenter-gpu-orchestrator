// Command potluck-gate holds two of the programs a release candidate is judged
// by before it replaces the live scheduler (README, "How a release is
// judged"). Both work on a COPY of a data folder that they make themselves;
// they never open the source folder for writing and never talk to a
// scheduler they did not start.
//
//	potluck-gate crash  (readiness and crash test of one build)
//	potluck-gate plans  (plan rows of two builds on the same data, and the way back)
//
// The third program, the drawing of every page, is scripts/gate/pages.mjs.
//
// Nothing about one fleet is built in: the data folder, the models folder,
// the program release, the fake fleet (a potluck-fake-agents --nodes file)
// and the builds are all given by the caller.
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `potluck-gate: release gate programs. Each works on a copy of a data folder.

usage:
  potluck-gate crash --scheduler BIN|image:NAME --data DIR --models DIR [flags]
  potluck-gate plans --old BIN|image:NAME --new BIN|image:NAME --data DIR --models DIR [--nodes FILE --fake-agents BIN] [flags]

Run "potluck-gate crash --help" or "potluck-gate plans --help" for the flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without the exit: 0 passed (or, for plans, ran), 1 a failed
// assertion, 2 a usage error or a run that could not be made.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "crash":
		return crashMain(args[1:], stdout, stderr)
	case "plans":
		return plansMain(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown command %q\n%s", args[0], usage)
	return 2
}
