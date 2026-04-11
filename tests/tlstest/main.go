package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	addr := "38.55.133.80:28766"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}

	fmt.Printf("Testing TLS to %s ...\n", addr)

	// TCP connect
	tcpConn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		fmt.Printf("TCP FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("TCP OK")

	// TLS handshake with h2 ALPN
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2"},
		MinVersion:         tls.VersionTLS13,
	}
	tlsConn := tls.Client(tcpConn, tlsCfg)
	err = tlsConn.Handshake()
	if err != nil {
		fmt.Printf("TLS with h2 ALPN FAIL: %v\n", err)
		tcpConn.Close()

		// Retry without ALPN
		fmt.Println("\nRetrying without ALPN...")
		tcpConn2, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			fmt.Printf("TCP FAIL: %v\n", err)
			os.Exit(1)
		}
		tlsCfg2 := &tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS13,
		}
		tlsConn2 := tls.Client(tcpConn2, tlsCfg2)
		err = tlsConn2.Handshake()
		if err != nil {
			fmt.Printf("TLS without ALPN FAIL: %v\n", err)
			tcpConn2.Close()

			// Retry with TLS 1.2
			fmt.Println("\nRetrying with TLS 1.2...")
			tcpConn3, _ := net.DialTimeout("tcp", addr, 5*time.Second)
			tlsCfg3 := &tls.Config{
				InsecureSkipVerify: true,
				MinVersion:         tls.VersionTLS12,
			}
			tlsConn3 := tls.Client(tcpConn3, tlsCfg3)
			err = tlsConn3.Handshake()
			if err != nil {
				fmt.Printf("TLS 1.2 FAIL: %v\n", err)
			} else {
				fmt.Printf("TLS 1.2 OK! Proto: %s, Version: 0x%04x\n", tlsConn3.ConnectionState().NegotiatedProtocol, tlsConn3.ConnectionState().Version)
			}
			return
		}
		fmt.Printf("TLS without ALPN OK! Proto: %s, Version: 0x%04x\n", tlsConn2.ConnectionState().NegotiatedProtocol, tlsConn2.ConnectionState().Version)
		return
	}

	cs := tlsConn.ConnectionState()
	fmt.Printf("TLS OK! Proto: %s, Version: 0x%04x\n", cs.NegotiatedProtocol, cs.Version)
}
