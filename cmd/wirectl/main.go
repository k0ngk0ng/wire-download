package main

import (
	"context"
	"fmt"
	"github.com/k0ngk0ng/wirectl/cli"
	"os"
	"os/exec"
)

var version = "dev"

func main() {
	if err := cli.RunRoot(context.Background(), os.Args[1:], version); err != nil {
		if _, childExit := err.(*exec.ExitError); !childExit {
			fmt.Fprintln(os.Stderr, "wirectl:", err)
		}
		os.Exit(cli.ExitCode(err))
	}
}
