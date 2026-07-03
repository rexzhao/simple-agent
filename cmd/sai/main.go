package main

import (
	"os"

	"github.com/rexzhao/simple-agent/internal/cli"
)

func main() {
	os.Exit(cli.RunWithProgram(os.Args[0], os.Args[1:], os.Stdout, os.Stderr))
}
