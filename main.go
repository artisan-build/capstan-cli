package main

import (
	"os"

	"github.com/artisan-build/capstan-cli/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
