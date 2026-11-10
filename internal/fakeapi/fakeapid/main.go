// Command fakeapid serves the fixtures of internal/fakeapi over HTTP, so the exporter can be
// started against it by hand. It is a development tool, not part of the release.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/CubePathInc/cubepath-k8s-exporter/internal/fakeapi"
)

func main() {
	fixture := flag.String("fixture", "testdata/fixture", "directory with the fixture files")
	listen := flag.String("listen", "127.0.0.1:0", "address to listen on")
	token := flag.String("token", "test-token", "bearer token the fake accepts")
	flag.Parse()

	srv, err := fakeapi.LoadFixture(*fixture, *token)
	if err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("fakeapid listening on http://" + ln.Addr().String())
	log.Fatal(http.Serve(ln, srv))
}
