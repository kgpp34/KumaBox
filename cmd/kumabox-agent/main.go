// Command kumabox-agent serves KumaBox's host-controlled guest command channel.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/kumabox/kumabox/agent"
	"github.com/kumabox/kumabox/version"
)

func main() {
	if len(os.Args) != 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := serve(); err != nil {
			fmt.Fprintf(os.Stderr, "kumabox-agent: %v\n", err)
			os.Exit(1)
		}
	case "version", "--version":
		fmt.Printf("kumabox-agent %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildTime)
	default:
		usage()
		os.Exit(2)
	}
}

func serve() error {
	listener, err := agent.ListenVsock(agent.Port)
	if err != nil {
		return err
	}
	server, err := agent.NewServer(listener, log.New(os.Stderr, "kumabox-agent: ", log.LstdFlags|log.Lmsgprefix))
	if err != nil {
		_ = listener.Close()
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.Serve(ctx)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kumabox-agent {serve|version}")
}
