package main

import (
	"os"
	"runtime/debug"

	"github.com/sh-lucas/vops/internal/cli"
)

var version = "dev" // set by -ldflags; `go install ...@vX` fills it from the module version

func main() {
	if bi, ok := debug.ReadBuildInfo(); ok && version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	cli.Version = version
	os.Exit(cli.Main(os.Args[1:]))
}
