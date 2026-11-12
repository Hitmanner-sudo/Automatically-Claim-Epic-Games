// Command mkres writes the resource object for the Windows program (see
// package winres). It is run before the program is built:
//
//	go run ./internal/winres/mkres -version 0.2.1 -o cmd/epicautoclaim/rsrc_windows_amd64.syso
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Hitmanner-sudo/Automatically-Claim-Epic-Games/internal/winres"
)

func main() {
	version := flag.String("version", "", "the program's version, e.g. 0.2.1")
	out := flag.String("o", "", "the file to write")
	flag.Parse()
	if *version == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: mkres -version 0.2.1 -o rsrc_windows_amd64.syso")
		os.Exit(2)
	}
	if err := os.WriteFile(*out, winres.Object(winres.Program(*version)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
