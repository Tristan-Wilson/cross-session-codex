package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/Tristan-Wilson/cross-session-codex/internal/release"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "csc-release:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, errOut io.Writer) error {
	var opts release.Options
	flags := flag.NewFlagSet("csc-release", flag.ContinueOnError)
	flags.SetOutput(errOut)
	flags.StringVar(&opts.Source, "source", ".", "Source repository (metadata is derived from Git)")
	flags.StringVar(&opts.Output, "out", "", "New package directory, native build file, or cross-build directory")
	flags.StringVar(&opts.Tag, "tag", "", "Exact clean HEAD version tag for a release package")
	flags.BoolVar(&opts.Snapshot, "snapshot", false, "Package a nonrelease snapshot of the current worktree")
	flags.BoolVar(&opts.Build, "build", false, "Build the native conventional binary without packaging")
	flags.BoolVar(&opts.CrossBuild, "cross-build", false, "Build four conventional platform binaries without packaging")
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	manifest, err := release.Run(ctx, opts)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(manifest)
}
