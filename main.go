// Command kumabox is the KumaBox command line.
//
// v1 has no daemon: every invocation opens the node root, takes the locks it
// needs, does one job, and exits (docs/DECISIONS.md DEC-018). This file does
// nothing but hand control to the command layer.
package main

import (
	"context"
	"os"

	"github.com/kumabox/kumabox/cmd"
)

func main() {
	os.Exit(cmd.Execute(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
