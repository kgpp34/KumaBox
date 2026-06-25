package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/kumabox/kumabox/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	switch args[0] {
	case "version":
		return runVersion(args[1:])
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runVersion(args []string) error {
	jsonOutput := false
	for _, arg := range args {
		switch arg {
		case "--json":
			jsonOutput = true
		case "-h", "--help":
			fmt.Println("Usage: kumabox version [--json]")
			return nil
		default:
			return fmt.Errorf("unknown version flag %q", arg)
		}
	}

	info := version.Info()
	if jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(info)
	}

	fmt.Printf("kumabox %s (%s, built %s)\n", info.Version, info.Commit, info.BuildTime)
	return nil
}

func printUsage() {
	fmt.Println("Usage: kumabox <command> [flags]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  version    Show version information")
}
