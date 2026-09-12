package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/k0ngk0ng/wire-download/internal/auth"
	"github.com/k0ngk0ng/wire-download/internal/client"
	"github.com/k0ngk0ng/wire-download/internal/config"
	"github.com/k0ngk0ng/wire-download/internal/daemon"
	"github.com/k0ngk0ng/wire-download/internal/tui"
	"github.com/k0ngk0ng/wirectl/cli"
	"golang.org/x/term"
)

var version = "dev"

func main() {
	syscall.Umask(0077)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "wirectl download:", tui.Clean(err.Error()))
		os.Exit(cli.ExitCode(err))
	}
}
func defaultDir() string {
	if d := os.Getenv("WIRECTL_DOWNLOAD_HOME"); d != "" {
		return d
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "wirectl", "download")
}
func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("wirectl download", flag.ContinueOnError)
	dir := flags.String("data-dir", defaultDir(), "State directory (or WIRECTL_DOWNLOAD_HOME)")
	flags.Usage = func() {}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			args = []string{"help"}
		} else {
			return err
		}
	} else {
		args = flags.Args()
	}
	absolute, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	*dir = absolute
	c := client.New(*dir)
	download := func(ctx context.Context, args []string) error {
		fs := flag.NewFlagSet("download", flag.ContinueOnError)
		detach := fs.Bool("detach", false, "Enqueue without opening the live dashboard")
		if err := fs.Parse(args); err != nil {
			return err
		}
		args = fs.Args()
		if len(args) == 0 {
			return errors.New("usage: wirectl download <link|file.torrent> [...]")
		}
		for _, source := range args {
			if !strings.Contains(source, ":") && strings.HasSuffix(strings.ToLower(source), ".torrent") {
				source, err = filepath.Abs(source)
				if err != nil {
					return err
				}
			}
			j, err := c.Add(ctx, source)
			if err != nil {
				return err
			}
			fmt.Printf("%s  %s  %s\n", j.ID, j.Engine, tui.Clean(j.Name))
		}
		if !*detach && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
			return tui.Watch(ctx, c)
		}
		return nil
	}
	app := cli.App{Name: "wirectl download", Description: "HTTP(S) / ed2k / BitTorrent / magnet downloads; pass a URL or torrent file directly\nGlobal option: --data-dir <path> (before the subcommand), or WIRECTL_DOWNLOAD_HOME", Default: download, Commands: map[string]cli.Command{}}
	app.Commands["add"] = cli.Command{Summary: "Enqueue links or torrent files", Run: download}
	app.Commands["login"] = cli.Command{Summary: "Browser login for HTTP(S), optionally --remote user@server", Run: func(ctx context.Context, args []string) error { return loginCommand(ctx, *dir, args) }}
	app.Commands["logout"] = cli.Command{Summary: "Forget a website's saved download session", Run: func(ctx context.Context, args []string) error {
		if len(args) != 1 {
			return errors.New("usage: wirectl download logout <website>")
		}
		return auth.Forget(*dir, args[0])
	}}
	app.Commands["init"] = cli.Command{Summary: "Create private configuration", Run: func(ctx context.Context, args []string) error {
		fs := flag.NewFlagSet("init", flag.ContinueOnError)
		home, _ := os.UserHomeDir()
		downloads := fs.String("downloads", filepath.Join(home, "Downloads", "wire-download"), "Download directory")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected init arguments")
		}
		if _, err := config.Init(*dir, *downloads); err != nil {
			return err
		}
		fmt.Println("Created", filepath.Join(*dir, "config.json"))
		return nil
	}}
	app.Commands["version"] = cli.Command{Summary: "Print version", Run: func(context.Context, []string) error { fmt.Println(version); return nil }}
	app.Commands["list"] = cli.Command{Summary: "List tasks (--json for automation)", Run: func(ctx context.Context, args []string) error {
		if len(args) > 1 || len(args) == 1 && args[0] != "--json" {
			return errors.New("usage: wirectl download list [--json]")
		}
		s, err := c.Status(ctx)
		if err != nil {
			return err
		}
		if len(args) == 1 {
			return json.NewEncoder(os.Stdout).Encode(s)
		}
		tui.Table(os.Stdout, s)
		return nil
	}}
	app.Commands["watch"] = cli.Command{Summary: "Interactive live transfer dashboard", Run: func(ctx context.Context, args []string) error {
		if len(args) != 0 {
			return errors.New("watch accepts no arguments")
		}
		return tui.Watch(ctx, c)
	}}
	for _, action := range []string{"pause", "resume", "remove"} {
		action := action
		app.Commands[action] = cli.Command{Summary: strings.ToUpper(action[:1]) + action[1:] + " tasks by ID", Run: func(ctx context.Context, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("usage: wirectl download %s <id> [...]", action)
			}
			for _, id := range args {
				if err := c.Action(ctx, id, action); err != nil {
					return err
				}
				fmt.Println(id, action)
			}
			return nil
		}}
	}
	app.Commands["daemon"] = cli.Command{Summary: "run | start | stop | status", Run: func(ctx context.Context, args []string) error { return daemonCommand(ctx, *dir, c, args) }}
	app.Commands["doctor"] = cli.Command{Summary: "Check configuration and installed engines", Run: func(ctx context.Context, args []string) error {
		cfg, err := config.Load(*dir)
		if err != nil {
			return err
		}
		failed := false
		for _, b := range []string{cfg.Aria2Binary, cfg.AMuledBinary, cfg.AMulecmdBinary} {
			p, err := exec.LookPath(b)
			if err != nil {
				fmt.Println("MISSING", b)
				failed = true
			} else {
				fmt.Println("OK", p)
			}
		}
		fmt.Println("State:", *dir)
		fmt.Println("Downloads:", cfg.Downloads)
		if failed {
			return errors.New("install aria2 and aMule (including amuled/amulecmd); see README")
		}
		return nil
	}}
	app.Commands["servers"] = cli.Command{Summary: "Update eMule server list while daemon is stopped", Run: func(ctx context.Context, args []string) error {
		if len(args) != 1 || args[0] != "update" {
			return errors.New("usage: wirectl download servers update")
		}
		return daemon.UpdateServers(ctx, *dir)
	}}
	return app.Run(ctx, args)
}
func daemonCommand(ctx context.Context, dir string, c *client.Client, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: wirectl download daemon run|start|stop|status")
	}
	switch args[0] {
	case "run":
		cfg, err := config.Load(dir)
		if err != nil {
			return err
		}
		return daemon.Run(ctx, dir, cfg)
	case "status":
		s, err := c.Status(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(s)
	case "stop":
		if err := c.Call(ctx, "POST", "/v1/shutdown", nil, nil); err != nil {
			return err
		}
		deadline := time.NewTimer(40 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			if _, err := os.Stat(filepath.Join(dir, "daemon.pid")); os.IsNotExist(err) {
				fmt.Println("Daemon stopped")
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				return errors.New("shutdown did not finish within 40s; inspect logs")
			case <-tick.C:
			}
		}
	case "start":
		if _, err := c.Status(ctx); err == nil {
			fmt.Println("Daemon already running")
			return nil
		}
		if _, err := config.Load(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dir, "logs"), 0700); err != nil {
			return err
		}
		logPath := filepath.Join(dir, "logs", "daemon.log")
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		self, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command(self, "--data-dir", dir, "daemon", "run")
		cmd.Stdout = f
		cmd.Stderr = f
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err = cmd.Start(); err != nil {
			return err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		timer := time.NewTimer(90 * time.Second)
		defer timer.Stop()
		tick := time.NewTicker(300 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case err = <-done:
				return fmt.Errorf("daemon startup failed (%v); see %s", err, logPath)
			case <-ctx.Done():
				_ = cmd.Process.Signal(syscall.SIGTERM)
				return ctx.Err()
			case <-timer.C:
				_ = cmd.Process.Signal(syscall.SIGTERM)
				return fmt.Errorf("daemon startup timed out; see %s", logPath)
			case <-tick.C:
				check, cancel := context.WithTimeout(ctx, time.Second)
				_, err = c.Status(check)
				cancel()
				if err == nil {
					fmt.Println("Daemon started")
					return nil
				}
			}
		}
	default:
		return errors.New("unknown daemon command")
	}
}
