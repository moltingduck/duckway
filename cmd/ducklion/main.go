package main

import (
	"os"

	"github.com/hackerduck/duckway/internal/ducklioncli"
)

func main() {
	ducklioncli.Main(os.Args[1:], os.Stdout)
}
