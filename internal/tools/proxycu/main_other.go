//go:build !darwin

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "verify-proxy-cu: process containment is supported only on Darwin")
	os.Exit(1)
}
