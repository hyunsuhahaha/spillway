// Command spillway is a single binary that runs every Spillway component:
//
//	spillway edge       public entry point / weighted proxy
//	spillway control    control plane + dashboard
//	spillway siteagent  Postgres supervisor for one site
//	spillway dbrouter   switchable Postgres TCP proxy
//	spillway probe      data-loss / RTO probe and load generator
//
// The web app itself is not part of Spillway: any image that follows the app
// contract (see examples/guestbook) runs as the local app and burst instances.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"spillway/internal/control"
	"spillway/internal/dbrouter"
	"spillway/internal/edge"
	"spillway/internal/probe"
	"spillway/internal/siteagent"
)

var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "edge":
		err = edge.New(edge.ConfigFromEnv()).Run(ctx)
	case "control":
		var cfg control.Config
		if cfg, err = control.ConfigFromEnv(); err == nil {
			err = control.New(cfg).Run(ctx)
		}
	case "siteagent":
		err = siteagent.New(siteagent.ConfigFromEnv()).Run(ctx)
	case "dbrouter":
		err = dbrouter.New(dbrouter.ConfigFromEnv()).Run(ctx)
	case "probe":
		err = probe.New(probe.ConfigFromEnv()).Run(ctx)
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("spillway %s: %v", os.Args[1], err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: spillway <edge|control|siteagent|dbrouter|probe|version>")
}
