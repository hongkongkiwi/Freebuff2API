package main

import (
	"io"
	"log"
)

// discardLogger returns a logger that writes nowhere, for tests.
func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}
