package main

import (
	"os"
	"testing"

	"github.com/hackerduck/duckway/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithShortTempRoot(m))
}
