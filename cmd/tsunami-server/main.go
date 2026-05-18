// TSUNAMI Server — main entry point
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	configfile "github.com/tsunami-protocol/tsunami/pkg/config"
	"github.com/tsunami-protocol/tsunami/pkg/fronting"
	"github.com/tsunami-protocol/tsunami/pkg/protocol"
	"github.com/tsunami-protocol/tsunami/pkg/server"
	"github.com/tsunami-protocol/tsunami/pkg/shaping"
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
		listen      = flag.String("listen", ":443", "Listen address")
		cert        = flag.String("cert", "", "TLS certificate file")
		key         = flag.String("key", "", "TLS private key file")
		password    = flag.String("password", "", "User password")
		configPath  = flag.String("config", "", "JSON config file")
		fallback    = flag.String("fallback", "", "Fallback backend address (e.g., 127.0.0.1:8080)")
		forceBBR    = flag.Bool("force-bbr", false, "Force per-connection TCP BBR on Linux")
		useFronting = flag.Bool("fronting", false, "Serve a Caddy-like HTTPS/HTTP2/WebSocket front")
		frontPath   = flag.String("front-path", fronting.DefaultPath, "Fronting HTTP path")
		frontSecret = flag.String("front-secret", "", "Fronting HTTP-layer secret (defaults to password)")
		frontSite   = flag.String("front-site-name", "Welcome", "Fronting decoy site name")
		frontDecoy  = flag.String("front-decoy-proxy", "", "Optional HTTP(S) origin for unauthenticated fronting requests")
		allowAll    = flag.Bool("allow-all", false, "Allow proxying to private/reserved IP addresses")
		trafficShaping = flag.Bool("traffic-shaping", false, "Enable constant-rate traffic shaping")
		showVersion = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("tsunami-server %s (commit=%s, built=%s)\n", version, commit, buildTime)
		os.Exit(0)
	}

	var config server.Config
	if *configPath != "" {
		fileConfig, err := configfile.LoadFile(*configPath)
		if err != nil {
			log.Fatalf("tsunami-server: load config: %v", err)
		}
		config = fileConfig.ToServerConfig()
	} else {
		if (*cert == "") != (*key == "") {
			log.Fatal("tsunami-server: --cert and --key must both be provided, or both omitted")
		}
		if *password == "" {
			log.Fatal("tsunami-server: --password is required")
		}

		config = server.Config{
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
			Fronting: fronting.Config{
				Enabled:  *useFronting,
				Path:     *frontPath,
				Secret:   *frontSecret,
				SiteName: *frontSite,
				DecoyProxy: *frontDecoy,
			},
		}
	}
	config.TCP.ForceBBR = *forceBBR
	config.AllowAll = *allowAll
	if *trafficShaping {
		shapingCfg := shaping.DefaultConfig()
		config.Shaping = &shapingCfg
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

	log.Printf("tsunami-server %s (commit=%s) starting...", version, commit)
	if err := srv.Start(); err != nil {
		log.Fatalf("tsunami-server: %v", err)
	}
}
