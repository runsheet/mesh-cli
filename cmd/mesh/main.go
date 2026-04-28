package main

import (
	"os"

	"projectmesh.io/mesh-cli/internal/commands"
)

func main() {
	if err := commands.Execute(); err != nil {
		os.Exit(1)
	}
}
