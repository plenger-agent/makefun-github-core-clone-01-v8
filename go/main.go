// The forge binary: a raw-socket HTTP server that carries the git
// smart HTTP, REST, GraphQL and MCP surfaces over one in-memory
// store.
//
// How it is run: in a trial the Node launcher (main.ts at the
// repository root) spawns this binary, reads the port it prints, and
// owns the register/poll lifecycle; this binary only serves. For
// local development it serves on FORGE_PORT (default 8793) until
// killed, so the kit's suite runner can dial it directly.
//
//	FORGE_PORT=0     bind an ephemeral port and print it, then serve.
//	FORGE_PORT=8793  bind 8793 and serve.
//	FORGE_ADDR       bind address (default 127.0.0.1).
//
// GC posture: the store's heap is small (tens of megabytes of objects
// and repositories) against the entry's 1 GiB limit, and the suite is
// a burst of short-lived per-request garbage, so the collector is
// told to wait four times longer between cycles than the default.
// That trades a little headroom the process never uses for measurably
// less GC CPU in the hot loop.

package main

import (
	"fmt"
	"net"
	"os"
	"runtime/debug"
	"strconv"
)

func main() {
	debug.SetGCPercent(400)

	engine := NewEngine()

	listenPort := 8793
	if text := os.Getenv("FORGE_PORT"); text != "" {
		if parsed, err := strconv.Atoi(text); err == nil {
			listenPort = parsed
		}
	}
	bindAddr := os.Getenv("FORGE_ADDR")
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}

	listener, err := net.Listen("tcp", bindAddr+":"+strconv.Itoa(listenPort))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	boundPort := listener.Addr().(*net.TCPAddr).Port
	fmt.Printf("[forge] listening on port %d\n", boundPort)

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go ServeConnection(conn, engine.Handle)
	}
}
