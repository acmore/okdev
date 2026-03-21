package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/acmore/okdev/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		if !errors.Is(err, cli.ErrProxyHealthDisconnect) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}
