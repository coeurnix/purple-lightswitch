package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"purple-lightswitch/internal/bootstrap"
	appRuntime "purple-lightswitch/internal/runtime"
	"purple-lightswitch/internal/tui"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "download-bins":
			return runDownloadBins(args[1:])
		case "download-models":
			return runDownloadModels(args[1:])
		}
	}
	return runServe(args)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("purple-lightswitch", flag.ContinueOnError)
	listenHost := fs.String("listen", bootstrap.DefaultListenHost(), "host/interface to bind")
	port := fs.Int("port", 0, "port to listen on; 0 means auto-pick starting at 27071")
	password := fs.String("password", "", "optional server password")
	interactive := fs.Bool("interactive", false, "open a startup form in the TUI before bootstrapping")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := appRuntime.Config{
		ListenHost: *listenHost,
		Port:       *port,
		Password:   *password,
	}
	return tui.Run(context.Background(), cfg, *interactive)
}

func runDownloadBins(args []string) error {
	fs := flag.NewFlagSet("download-bins", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "show planned work without downloading")
	all := fs.Bool("all", false, "download all supported stable-diffusion.cpp targets")
	target := fs.String("target", "", "download only one target directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	return bootstrap.DownloadBins(context.Background(), bootstrap.BinsOptions{
		DryRun: *dryRun,
		All:    *all,
		Target: *target,
	}, consoleReporter())
}

func runDownloadModels(args []string) error {
	fs := flag.NewFlagSet("download-models", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "show planned work without downloading")
	force := fs.Bool("force", false, "re-download model files even if they already exist")
	if err := fs.Parse(args); err != nil {
		return err
	}

	return bootstrap.DownloadModels(context.Background(), bootstrap.ModelsOptions{
		DryRun: *dryRun,
		Force:  *force,
	}, consoleReporter())
}

func consoleReporter() bootstrap.Reporter {
	return bootstrap.Reporter{
		Log: func(message string) {
			fmt.Println(message)
		},
		Progress: func(item bootstrap.Progress) {
			if item.Done {
				fmt.Printf("%s %s complete\n", item.Label, item.Phase)
			}
		},
	}
}
