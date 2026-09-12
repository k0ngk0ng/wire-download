package cli

import (
	"context"
	"fmt"
)

// RunRoot is the shared entry point used by standalone and bundled hosts.
func RunRoot(ctx context.Context, args []string, version string) error {
	app := App{Name: "wirectl", Description: "small tools, independent commands", PluginPrefix: "wirectl-", Commands: map[string]Command{
		"version": {Summary: "Print version", Run: func(context.Context, []string) error { fmt.Println(version); return nil }},
	}}
	return app.Run(ctx, args)
}
