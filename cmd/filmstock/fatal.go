package main

import (
	"fmt"
	"os"
)

// fatal is local to this command. internal/ packages return errors instead —
// only a main is entitled to end the process.
func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
}
