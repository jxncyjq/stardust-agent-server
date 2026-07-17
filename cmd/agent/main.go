package main

import (
	"fmt"
	"os"

	"github.com/stardust/legion-agent/internal/app"
	"github.com/stardust/legion-agent/internal/cli"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	return cli.Execute(app.New(), os.Stdout, os.Args[1:])
}
