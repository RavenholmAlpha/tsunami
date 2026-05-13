// TSUNAMI Client — local SOCKS5/HTTP proxy entry point
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/tsunami-protocol/tsunami/pkg/client"
	"github.com/tsunami-protocol/tsunami/pkg/fronting"
	"github.com/tsunami-protocol/tsunami/pkg/mux"
	"github.com/tsunami-protocol/tsunami/pkg/proxy"
	"github.com/tsunami-protocol/tsunami/pkg/surge"
	"github.com/tsunami-protocol/tsunami/pkg/transport"
)

// Populated at build time via -ldflags
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	var (
		server         = flag.String("server", "", "TSUNAMI server address (host:port)")
		password       = flag.String("password", "", "Authentication password")
		sni            = flag.String("sni", "", "TLS SNI (defaults to server hostname)")
		skipVerify     = flag.Bool("skip-verify", false, "Skip TLS certificate verification")
		socksAddr      = flag.String("socks", "127.0.0.1:1080", "Local SOCKS5 proxy address")
		httpAddr       = flag.String("http", "127.0.0.1:8080", "Local HTTP proxy address")
		maxConn        = flag.Int("max-connections", 4, "Surge max connections")
		threshold      = flag.Int("threshold", 8, "Surge stream threshold for Layer 2")
		fingerprint    = flag.String("fingerprint", "chrome", "TLS fingerprint: chrome, firefox, safari, random, none")
		forceBBR       = flag.Bool("force-bbr", false, "Force per-connection TCP BBR on Linux")
		useFronting    = flag.Bool("fronting", false, "Use HTTPS/HTTP2/WebSocket fronting transport")
		frontPath      = flag.String("front-path", fronting.DefaultPath, "Fronting HTTP path")
		frontHost      = flag.String("front-host", "", "Fronting Host header (defaults to SNI)")
		frontSecret    = flag.String("front-secret", "", "Fronting HTTP-layer secret (defaults to password)")
		frontTransport = flag.String("front-transport", fronting.TransportH2, "Fronting transport: h2 or websocket")
		showVersion    = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("tsunami-client %s (commit=%s, built=%s)\n", version, commit, buildTime)
		os.Exit(0)
	}

	if *server == "" || *password == "" {
		log.Fatal("tsunami-client: --server and --password are required")
	}

	sniName := *sni
	if sniName == "" {
		// Extract hostname from server address
		host, _, _ := splitHostPort(*server)
		sniName = host
	}

	cfg := client.Config{
		ServerAddr: *server,
		Password:   *password,
		TLS: transport.TLSConfig{
			ServerName:  sniName,
			SkipVerify:  *skipVerify,
			Fingerprint: *fingerprint,
			ALPN:        []string{"h2"},
			MinVersion:  tls.VersionTLS13,
		},
		TCP: *transport.DefaultTCPConfig(),
		Surge: surge.Config{
			Mode:           surge.ModeAuto,
			MaxConnections: *maxConn,
			Threshold:      *threshold,
		},
		Mux: mux.DefaultPoolConfig(),
		Fronting: fronting.Config{
			Enabled:   *useFronting,
			Path:      *frontPath,
			Host:      *frontHost,
			Secret:    *frontSecret,
			Transport: *frontTransport,
		},
		UDP: true,
	}
	cfg.TCP.ForceBBR = *forceBBR

	c, err := client.New(cfg)
	if err != nil {
		log.Fatalf("tsunami-client: %v", err)
	}

	// Start SOCKS5 proxy
	socks5 := proxy.NewSOCKS5Server(c)
	go func() {
		if err := socks5.ListenAndServe(*socksAddr); err != nil {
			log.Printf("tsunami-client: socks5: %v", err)
		}
	}()

	// Start HTTP proxy
	httpProxy := proxy.NewHTTPProxyServer(c)
	go func() {
		if err := httpProxy.ListenAndServe(*httpAddr); err != nil {
			log.Printf("tsunami-client: http proxy: %v", err)
		}
	}()

	log.Printf("tsunami-client: SOCKS5 proxy on %s", *socksAddr)
	log.Printf("tsunami-client: HTTP proxy on %s", *httpAddr)

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("tsunami-client: shutting down...")
	socks5.Close()
	httpProxy.Close()
	c.Close()
}

func splitHostPort(addr string) (string, string, error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], nil
		}
	}
	return addr, "", nil
}
