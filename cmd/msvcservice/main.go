// Command msvcservice is the service binary. One image, two shapes:
//
//	msvcservice serve    HTTP API (+ consumers, outbox dispatcher and task runner by configuration)
//	msvcservice worker   background only: probes and metrics, consumers, dispatcher, task runner
//	msvcservice version  print the build version
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"example.com/msvcservice/api"
	"example.com/msvcservice/internal/app"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	command := "serve"
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "serve", "worker":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := app.Run(ctx, app.Mode(command), version, api.OpenAPI); err != nil {
			fmt.Fprintln(os.Stderr, "fatal:", err)
			return 1
		}
		return 0
	case "version":
		fmt.Println(version)
		return 0
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", command)
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: msvcservice [serve|worker|version]

There are no flags: configuration comes from the environment.
  APP_ENV=development   applies internal/config/development.jsonc (unset means production)
  SECTION__KEY=value    overrides any configuration key, e.g. SERVER__ADDR=:9090
`)
}
