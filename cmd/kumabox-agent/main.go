package main

import (
	"fmt"
	"os"

	"github.com/kumabox/kumabox/internal/guestagent"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := guestagent.Serve(); err != nil {
			fmt.Fprintf(os.Stderr, "kumabox-agent: %v\n", err)
			os.Exit(1)
		}
	case "version", "--version":
		fmt.Println(guestagent.Version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kumabox-agent {serve|version}")
}
