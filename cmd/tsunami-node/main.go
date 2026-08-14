// TSUNAMI Node is a panel-managed standalone backend for private deployments.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	configfile "github.com/tsunami-protocol/tsunami/pkg/config"
	"github.com/tsunami-protocol/tsunami/pkg/node"
)

// Populated at build time via -ldflags.
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	configPath := flag.String("config", "/etc/tsunami/node.json", "Path to the TSUNAMI node JSON configuration")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("tsunami-node %s (commit=%s, built=%s)\n", version, commit, buildTime)
		return
	}

	fileConfig, err := configfile.LoadFile(*configPath)
	if err != nil {
		log.Fatalf("tsunami-node: load config: %v", err)
	}
	nodeConfig, err := fileConfig.ToNodeConfig()
	if err != nil {
		log.Fatalf("tsunami-node: build config: %v", err)
	}
	instance, err := node.New(nodeConfig)
	if err != nil {
		log.Fatalf("tsunami-node: initialize: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for signal := range signals {
			if signal == syscall.SIGHUP {
				log.Println("tsunami-node: synchronizing with XBoard...")
				syncCtx, syncCancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := instance.SyncNow(syncCtx); err != nil {
					log.Printf("tsunami-node: sync: %v", err)
				} else {
					log.Println("tsunami-node: XBoard sync completed")
				}
				syncCancel()
				continue
			}

			log.Println("tsunami-node: shutting down...")
			cancel()
			if err := instance.Close(); err != nil {
				log.Printf("tsunami-node: close: %v", err)
			}
			return
		}
	}()

	log.Printf("tsunami-node %s (commit=%s) starting...", version, commit)
	if err := instance.Start(ctx); err != nil {
		log.Fatalf("tsunami-node: %v", err)
	}
}
