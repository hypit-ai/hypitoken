// Standalone binary: no access to HypiToken's provider credentials or database.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/wjsoj/CPA-Claude/internal/gptpay"
)

func main() {
	// run's own `defer stop()` must actually execute on every exit path, so
	// this is the only place log.Fatal is allowed to run — nothing here is
	// deferred, so os.Exit skipping deferred calls costs nothing.
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:8321", "listen address behind HTTPS reverse proxy")
	flag.Parse()
	service, err := gptpay.ServiceFromEnv()
	if err != nil {
		return err
	}
	s := &http.Server{Addr: *addr, Handler: gptpay.NewHandler(service), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 4 * time.Minute, IdleTimeout: time.Minute, MaxHeaderBytes: 16384}
	log.Printf("GPTPay listening on %s; payment enabled: %t", *addr, service != nil)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe() }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		stop()
		// Let in-flight payments finish before systemd stops the process.
		shutdown, cancel := context.WithTimeout(context.Background(), 200*time.Second)
		defer cancel()
		if err := s.Shutdown(shutdown); err != nil {
			log.Print("GPTPay shutdown timed out; reconcile payment records before retrying")
		}
		return nil
	}
}
