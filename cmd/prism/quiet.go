package main

import (
	"io"
	"os"
)

func quiet() bool { return os.Getenv("PRISM_QUIET") == "1" }

// envOut is where `prism up` reprints the exports. They are for a shell
// to eval, and a quiet caller has no shell reading stdout.
func envOut() io.Writer {
	if quiet() {
		return io.Discard
	}
	return os.Stdout
}

// childStdio is for children such as `tsh app login`. A quiet caller's
// stdin belongs to the tool, so they get the null device (nil) and a
// prompt fails instead of waiting.
func childStdio() (io.Reader, io.Writer) {
	if quiet() {
		return nil, os.Stderr
	}
	return os.Stdin, os.Stdout
}
