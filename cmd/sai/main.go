package main

import (
	"os"

	"github.com/rexzhao/simple-agent/internal/webapp"
)

func main() {
	os.Exit(webapp.Run(os.Args[1:], os.Stdout, os.Stderr))
}
