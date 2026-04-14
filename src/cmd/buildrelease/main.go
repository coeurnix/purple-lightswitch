package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

type target struct {
	goos   string
	goarch string
	output string
}

var targets = []target{
	{goos: "windows", goarch: "amd64", output: "PurpleLightswitch-win-x64.exe"},
	{goos: "linux", goarch: "amd64", output: "PurpleLightswitch-linux-x64"},
	{goos: "darwin", goarch: "arm64", output: "PurpleLightswitch-osx-arm64"},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	srcRoot, err := os.Getwd()
	if err != nil {
		return err
	}
	repoRoot := filepath.Dir(srcRoot)

	for _, item := range targets {
		outputPath := filepath.Join(repoRoot, item.output)
		fmt.Printf("Building %s/%s -> %s\n", item.goos, item.goarch, item.output)

		cmd := exec.Command("go", "build", "-trimpath", "-o", outputPath, "./cmd/purple-lightswitch")
		cmd.Dir = srcRoot
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(),
			"CGO_ENABLED=0",
			"GOOS="+item.goos,
			"GOARCH="+item.goarch,
		)

		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build failed for %s/%s: %w", item.goos, item.goarch, err)
		}

		if runtime.GOOS != "windows" && item.goos != "windows" {
			_ = os.Chmod(outputPath, 0o755)
		}
	}

	fmt.Println("Release binaries written to repo root.")
	return nil
}
