// Command whatbark is a single-binary AI team workspace:
// serve (platform), agent (local executor), bark (agent CLI), preset
// (org-structure presets: list / import with token issuance).
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "agent":
		cmdAgent(os.Args[2:])
	case "bark":
		cmdBark(os.Args[2:])
	case "preset":
		cmdPreset(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: whatbark <serve|agent|bark|preset> [flags]")
}
