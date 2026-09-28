// Command agentboard is a live board of coding-agent sessions and the
// decisions they wait on. See README.md.
package main

import (
	"context"
	"os"

	"github.com/barelyworkingcode/agentboard/internal/cli"
	"github.com/barelyworkingcode/agentboard/internal/client"
	"github.com/barelyworkingcode/agentboard/internal/hook"
	"github.com/barelyworkingcode/agentboard/internal/server"
	"github.com/barelyworkingcode/agentboard/web"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			return server.Main(ctx, args[1:], web.FS, os.Stderr)
		case "hook":
			return hook.Main(ctx, os.Stdin, client.EnvFromOS(), os.Stderr)
		}
	}
	return cli.Main(ctx, args, client.EnvFromOS(), os.Stdout, os.Stderr)
}
