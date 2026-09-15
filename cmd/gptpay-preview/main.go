// Local preview without loading HypiToken config, credentials or databases.
// Production uses the very same handler embedded in cmd/server.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/wjsoj/CPA-Claude/internal/gptpay"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8321", "preview listen address (never enables payment)")
	flag.Parse()
	s := &http.Server{Addr: *addr, Handler: gptpay.NewHandler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
	log.Printf("GPTPay preview: http://%s", *addr)
	log.Fatal(s.ListenAndServe())
}
