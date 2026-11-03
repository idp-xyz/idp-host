// Command fakemodel serves the deterministic model double on a TCP address, for the compose
// end-to-end topology (the HOST container reaches it through host.docker.internal).
//
//	IDP_FAKEMODEL_LISTEN   default 0.0.0.0:18080
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"idp-host/tests/fixtures/fakemodel"
)

func main() {
	addr := os.Getenv("IDP_FAKEMODEL_LISTEN")
	if addr == "" {
		addr = "0.0.0.0:18080"
	}
	srv := &http.Server{Addr: addr, Handler: fakemodel.New().Handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("fakemodel (TEST DOUBLE) listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
