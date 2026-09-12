// Package cli provides command routing for independently released wirectl plugins.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Command struct {
	Summary string
	Run     func(context.Context, []string) error
}
type App struct {
	Name        string
	Description string
	Commands    map[string]Command
	Default     func(context.Context, []string) error
	// PluginPrefix enables PATH dispatch; only the root app should set it.
	PluginPrefix string
	Out          io.Writer
}

var commandName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func (a App) Run(ctx context.Context, args []string) error {
	if a.Out == nil {
		a.Out = os.Stdout
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintf(a.Out, "%s — %s\n\nUsage: %s <command> [arguments]\n", a.Name, a.Description, a.Name)
		names := make([]string, 0, len(a.Commands))
		for name := range a.Commands {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(a.Out, "  %-12s %s\n", name, a.Commands[name].Summary)
		}
		if a.PluginPrefix != "" {
			for _, name := range plugins(a.PluginPrefix) {
				if _, known := a.Commands[name]; !known {
					fmt.Fprintf(a.Out, "  %-12s Installed plugin\n", name)
				}
			}
			fmt.Fprintf(a.Out, "\nInstalled commands are discovered as %s<command> on PATH.\n", a.PluginPrefix)
		}
		return nil
	}
	if cmd, ok := a.Commands[args[0]]; ok {
		return cmd.Run(ctx, args[1:])
	}
	if a.PluginPrefix != "" && commandName.MatchString(args[0]) {
		plugin := a.PluginPrefix + args[0]
		path, err := exec.LookPath(plugin)
		if self, selfErr := os.Executable(); selfErr == nil {
			if resolved, resolveErr := filepath.EvalSymlinks(self); resolveErr == nil {
				self = resolved
			}
			sibling := filepath.Join(filepath.Dir(self), plugin)
			if st, statErr := os.Stat(sibling); statErr == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0 {
				path, err = sibling, nil
			}
		}
		if err != nil {
			return fmt.Errorf("command %q is not installed; install %s%s and place it on PATH", args[0], a.PluginPrefix, args[0])
		}
		cmd := exec.CommandContext(ctx, path, args[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	if a.Default != nil {
		return a.Default(ctx, args)
	}
	return fmt.Errorf("unknown command %q (use %s --help)", args[0], a.Name)
}

func plugins(prefix string) []string {
	dirs := filepath.SplitList(os.Getenv("PATH"))
	if self, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		dirs = append(dirs, filepath.Dir(self))
	}
	found := map[string]bool{}
	seen := map[string]bool{}
	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := strings.TrimPrefix(entry.Name(), prefix)
			if name == entry.Name() || !commandName.MatchString(name) {
				continue
			}
			if st, err := os.Stat(filepath.Join(dir, entry.Name())); err == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0 {
				found[name] = true
			}
		}
	}
	names := []string{}
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := err.(*exec.ExitError); ok && e.ExitCode() > 0 {
		return e.ExitCode()
	}
	return 1
}
