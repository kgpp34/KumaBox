// Command kumabox is the KumaBox command line.
//
// v1 has no daemon: every invocation opens the node root, does one job and
// exits. This file only hands control to the command layer and turns the result
// into a process exit code.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kumabox/kumabox/cmd"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := cmd.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if err != nil && !cmd.Silent(err) {
		fmt.Fprintf(os.Stderr, "kumabox: %v\n", err)
	}
	stop()
	os.Exit(cmd.ExitCode(err))
}
