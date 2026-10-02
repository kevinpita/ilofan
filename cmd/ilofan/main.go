package main

import (
	"fmt"
	"os"

	"github.com/kevinpita/ilofan/internal/cli"
)

func main() {
	if err := cli.NewCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "ilofan:", err)
		os.Exit(1)
	}
}
