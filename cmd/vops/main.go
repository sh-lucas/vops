package main

import (
	"os"

	"github.com/sh-lucas/vops/internal/cli"
)

var version = "dev"

func main() {
	cli.Version = version
	os.Exit(cli.Main(os.Args[1:]))
}
