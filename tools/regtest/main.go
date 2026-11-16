// regtest runs xu's in-memory test registry on a fixed address, for trying
// `xu build --push` and XU_MIRRORS locally without a real registry:
//
//	go run ./tools/regtest -addr 127.0.0.1:5000
//	xu build . --push localhost:5000/me/app:dev
//
// Everything is lost when it stops.
package main

import (
	"flag"
	"log"
	"net/http"

	"xu526/internal/bundle/registry/regtest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:5000", "address to listen on")
	flag.Parse()
	log.Printf("test registry on http://%s (in memory)", *addr)
	log.Fatal(http.ListenAndServe(*addr, regtest.NewHandler()))
}
