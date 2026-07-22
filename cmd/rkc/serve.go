package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/repository-knowledge-compiler/rkc/internal/server"
)

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	addr := fs.String("addr", "127.0.0.1:8787", "HTTP listen address")
	readTimeout := fs.Duration("read-timeout", 15*time.Second, "HTTP read timeout")
	writeTimeout := fs.Duration("write-timeout", 60*time.Second, "HTTP write timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dataset, err := server.Load(*dir)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Addr: *addr, Handler: dataset.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: *readTimeout, WriteTimeout: *writeTimeout, IdleTimeout: 60 * time.Second}
	fmt.Printf("RKC snapshot %s at http://%s\n", dataset.Manifest.ID, *addr)
	return httpServer.ListenAndServe()
}
