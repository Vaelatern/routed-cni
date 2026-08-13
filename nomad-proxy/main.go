// nomad-proxy: unix socket → Nomad HTTP API with X-Nomad-Token injected.
//
// Usage:
//   export NOMAD_TOKEN=...
//   export NOMAD_ADDR=http://127.0.0.1:4646   # optional
//   ./nomad-proxy -sock /tmp/nomad.sock
//
// Client: NOMAD_ADDR=unix:///tmp/nomad.sock  (or HTTP client pointed at the socket)
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	sock := flag.String("sock", "/tmp/nomad-proxy.sock", "unix socket path")
	addr := flag.String("addr", env("NOMAD_ADDR", "http://127.0.0.1:4646"), "nomad HTTP address")
	flag.Parse()

	token := os.Getenv("NOMAD_TOKEN")
	if token == "" {
		log.Fatal("NOMAD_TOKEN required")
	}

	target, err := url.Parse(*addr)
	if err != nil {
		log.Fatal(err)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
			// always our token; drop any client-supplied one
			r.Out.Header.Set("X-Nomad-Token", token)
		},
	}

	_ = os.Remove(*sock)
	ln, err := net.Listen("unix", *sock)
	if err != nil {
		log.Fatal(err)
	}
	// world-readable socket is caller's problem; default 0660-ish via umask
	if err := os.Chmod(*sock, 0o660); err != nil {
		log.Fatal(err)
	}

	srv := &http.Server{Handler: proxy}
	go func() {
		log.Printf("listening on %s → %s", *sock, *addr)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	_ = srv.Shutdown(context.Background())
	_ = os.Remove(*sock)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
