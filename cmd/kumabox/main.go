// Command kumabox is the KumaBox command line.
//
// This file hands control to the command layer and turns the result into a
// process exit code. The optional daemon command uses the same entry point.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kumabox/kumabox/cli"
	"github.com/kumabox/kumabox/vmm/firecracker"
)

// main propagates termination signals, prints unhandled diagnostics, and exits
// with the status selected by the CLI after command resource cleanup has finished.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var err error
	if len(os.Args) == 4 && os.Args[1] == firecracker.ConsoleRelayCommand && os.Args[2] == "--api-socket" {
		err = firecracker.RunConsoleRelay(ctx)
	} else {
		err = cli.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	}
	if err != nil && !cli.Silent(err) {
		fmt.Fprintf(os.Stderr, "kumabox: %v\n", err)
	}
	stop()
	os.Exit(cli.ExitCode(err))
}
