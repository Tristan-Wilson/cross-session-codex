package main

import (
	"os"

	"github.com/Tristan-Wilson/cross-session-codex/internal/bridge"
)

func main() { os.Exit(bridge.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
