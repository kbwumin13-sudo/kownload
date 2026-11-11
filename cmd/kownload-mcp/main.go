// kownload-mcp is a read-only stdio adapter. It never owns downloader state.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		// Never print errors containing paths, request data or backend responses.
		if errors.Is(err, errInstallDurability) {
			fmt.Fprintln(os.Stderr, "kownload-mcp: helper published; durability not confirmed")
		} else {
			fmt.Fprintln(os.Stderr, "kownload-mcp: session ended")
		}
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "install-helper" {
		return runInstall(os.Args[2:])
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("kownload-mcp", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	socket := flags.String("socket", filepath.Join(home, "Library", "Application Support", "io.github.kbwumin13-sudo.kownload", "ipc", "api.sock"), "trusted application socket")
	tokenFile := flags.String("api-token-file", "", "explicit private API token file (read once)")
	showVersion := flags.Bool("version", false, "print bridge version")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *showVersion {
		fmt.Println("kownload-mcp " + version + " read-api=1")
		return nil
	}
	if !filepath.IsAbs(*socket) || filepath.Clean(*socket) != *socket {
		return fmt.Errorf("invalid socket")
	}
	var apiToken string
	var tokenFileSet bool
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "api-token-file" {
			tokenFileSet = true
		}
	})
	if tokenFileSet {
		apiToken, err = readAPITokenFile(*tokenFile)
		if err != nil {
			return err
		}
	}
	backend := newBackend(*socket, apiToken)
	defer backend.close()
	server := newServer(backend)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	input, err := pollableFile(os.Stdin)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := pollableFile(os.Stdout)
	if err != nil {
		return err
	}
	defer output.Close()
	return server.Run(ctx, &mcp.IOTransport{
		Reader: newLineReader(input), Writer: &boundedWriter{file: output},
	})
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
