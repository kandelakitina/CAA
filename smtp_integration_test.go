//go:build mailintegration

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"
)

func TestEmailSMTPTransport(t *testing.T) {
	if os.Getenv("NEVA_MAIL_INTEGRATION") != "1" {
		t.Skip("local sockets require NEVA_MAIL_INTEGRATION=1")
	}
	certificateServer := httptest.NewTLSServer(nil)
	certificates := certificateServer.TLS.Certificates
	roots := x509.NewCertPool()
	roots.AddCert(certificateServer.Certificate())
	certificateServer.Close()
	for _, mode := range []string{"tls", "starttls", "missing-starttls", "untrusted-tls"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			received := make(chan string, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					received <- "accept failed"
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				secured := false
				if mode == "tls" || mode == "untrusted-tls" {
					tc := tls.Server(conn, &tls.Config{Certificates: certificates, MinVersion: tls.VersionTLS12})
					if err = tc.Handshake(); err != nil {
						received <- "handshake refused"
						return
					}
					conn = tc
					secured = true
				}
				reader := textproto.NewReader(bufio.NewReader(conn))
				fmt.Fprint(conn, "220 test SMTP\r\n")
				for {
					line, err := reader.ReadLine()
					if err != nil {
						received <- "closed"
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO"):
						if !secured && mode != "missing-starttls" {
							fmt.Fprint(conn, "250-test\r\n250 STARTTLS\r\n")
						} else {
							fmt.Fprint(conn, "250-test\r\n250 AUTH PLAIN\r\n")
						}
					case line == "STARTTLS":
						fmt.Fprint(conn, "220 ready\r\n")
						tc := tls.Server(conn, &tls.Config{Certificates: certificates, MinVersion: tls.VersionTLS12})
						if err = tc.Handshake(); err != nil {
							received <- "handshake refused"
							return
						}
						conn = tc
						secured = true
						reader = textproto.NewReader(bufio.NewReader(conn))
					case strings.HasPrefix(line, "AUTH"):
						if !secured {
							received <- "plaintext auth"
							return
						}
						fmt.Fprint(conn, "235 accepted\r\n")
					case strings.HasPrefix(line, "MAIL FROM:"), strings.HasPrefix(line, "RCPT TO:"):
						fmt.Fprint(conn, "250 accepted\r\n")
					case line == "DATA":
						fmt.Fprint(conn, "354 send data\r\n")
						data, err := reader.ReadDotBytes()
						if err != nil {
							received <- "data failed"
							return
						}
						fmt.Fprint(conn, "250 queued\r\n")
						received <- string(data)
					case line == "QUIT":
						fmt.Fprint(conn, "221 bye\r\n")
						return
					default:
						received <- "unexpected command"
						return
					}
				}
			}()
			host, port, _ := net.SplitHostPort(listener.Addr().String())
			security := mode
			if mode == "missing-starttls" {
				security = "starttls"
			}
			if mode == "untrusted-tls" {
				security = "tls"
			}
			cfg := &mailConfig{Host: host, Port: port, Mode: security, From: "sender@example.com", Username: "sender@example.com", Password: "test-only"}
			tc := &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12}
			if mode == "untrusted-tls" {
				tc.RootCAs = x509.NewCertPool()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = cfg.sendTLS(ctx, "recipient@example.com", "Test subject", "Test body", "test@example.com", tc)
			result := <-received
			if mode == "missing-starttls" || mode == "untrusted-tls" {
				if err == nil || result == "plaintext auth" {
					t.Fatal("unsafe SMTP connection accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("SMTP transport failed: %v", err)
			}
			if !strings.Contains(result, "To: recipient@example.com") || !strings.Contains(result, "Message-ID: <test@example.com>") {
				t.Fatal("SMTP did not receive the message")
			}
		})
	}
}
