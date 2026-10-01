// Command dpx-mapctl creates a fresh content-alias store for one DPX daemon.
// The Studio unit (studio/dpx_unit.py) runs it once per start, inside the
// unit's RAM state directory, before CPA boots.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentalias"
)

func main() {
	dir := flag.String("init", "", "store directory to create (must be empty or absent)")
	principal := flag.String("principal", "", "binding principal")
	session := flag.String("session", "", "binding session id")
	version := flag.String("version", "v1", "binding version")
	flag.Parse()
	if *dir == "" || *principal == "" || *session == "" {
		fmt.Fprintln(os.Stderr, "usage: dpx-mapctl --init DIR --principal P --session S [--version v1]")
		os.Exit(2)
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		os.Exit(1)
	}
	b := contentalias.Binding{Principal: *principal, Session: *session, Version: *version}
	if _, err := contentalias.Create(*dir, b, contentalias.DefaultManifest()); err != nil {
		fmt.Fprintln(os.Stderr, "create:", err)
		os.Exit(1)
	}
	fmt.Println("created", *dir)
}
