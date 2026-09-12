package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/k0ngk0ng/wire-download/internal/auth"
)

func loginCommand(ctx context.Context, dir string, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	browser := fs.String("browser", "", "Installed Chrome/Chromium executable")
	remote := fs.String("remote", "", "Save the login on an SSH host (user@server)")
	importSession := fs.Bool("import", false, "Receive a private session over stdin (used by SSH transfer)")
	var target string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if target == "" && fs.NArg() == 1 {
		target = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return errors.New("usage: wirectl download login <website> [--browser path] [--remote user@server]")
	}
	if *importSession {
		if target != "" || *remote != "" || *browser != "" {
			return errors.New("login --import accepts only session JSON on stdin")
		}
		b, err := io.ReadAll(io.LimitReader(os.Stdin, (1<<20)+1))
		if err != nil {
			return err
		}
		if len(b) > 1<<20 {
			return errors.New("login session exceeds 1 MiB")
		}
		var session auth.Session
		if json.Unmarshal(b, &session) != nil {
			return errors.New("invalid login session")
		}
		if err = auth.Save(dir, session); err != nil {
			return err
		}
		fmt.Println(auth.Describe(session))
		return nil
	}
	if _, err := auth.Origin(target); err != nil {
		return err
	}
	if *remote != "" && !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:\[\]-]*$`).MatchString(*remote) {
		return errors.New("remote must be an SSH host or user@host; use ~/.ssh/config for connection options")
	}
	session, err := auth.Capture(ctx, dir, target, auth.BrowserOptions{Binary: *browser}, func() error {
		fmt.Println("Complete the website login in the browser, then press Enter here to save it. Ctrl-C cancels.")
		read := make(chan error, 1)
		go func() { _, err := bufio.NewReader(os.Stdin).ReadString('\n'); read <- err }()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-read:
			return err
		}
	})
	if err != nil {
		return err
	}
	if *remote != "" {
		data, _ := json.Marshal(session)
		cmd := exec.CommandContext(ctx, "ssh", "-T", *remote, "wirectl download login --import")
		cmd.Stdin = bytes.NewReader(data)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	if err = auth.Save(dir, session); err != nil {
		return err
	}
	fmt.Println(auth.Describe(session))
	return nil
}
