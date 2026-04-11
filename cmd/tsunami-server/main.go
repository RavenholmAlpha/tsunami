// TSUNAMI Server — main entry point
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/server"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
)

func main() {
	var (
		listen   = flag.String("listen", ":443", "Listen address")
		cert     = flag.String("cert", "", "TLS certificate file")
		key      = flag.String("key", "", "TLS private key file")
		password = flag.String("password", "", "User password")
		fallback = flag.String("fallback", "", "Fallback backend address (e.g., 127.0.0.1:8080)")
	)
	flag.Parse()

	if *cert == "" || *key == "" {
		log.Fatal("tsunami-server: --cert and --key are required")
	}
	if *password == "" {
		log.Fatal("tsunami-server: --password is required")
	}

	config := server.Config{
		Listen: *listen,
		TLS: transport.TLSConfig{
			CertFile:   *cert,
			KeyFile:    *key,
			ALPN:       []string{"h2"},
			MinVersion: 0x0304, // TLS 1.3
		},
		TCP: *transport.DefaultTCPConfig(),
		Users: []*protocol.UserInfo{
			{
				Name:     "default",
				Password: *password,
			},
		},
		FallbackAddr: *fallback,
	}

	srv, err := server.New(config)
	if err != nil {
		log.Fatalf("tsunami-server: %v", err)
	}

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("tsunami-server: shutting down...")
		srv.Close()
		os.Exit(0)
	}()

	log.Println("tsunami-server: starting...")
	if err := srv.Start(); err != nil {
		log.Fatalf("tsunami-server: %v", err)
	}
}
