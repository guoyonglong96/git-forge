// Command forge is the Go entrypoint for the make.fun Forge server.
//
// It ONLY binds an ephemeral port, prints the listening line, and serves.
// The trial lifecycle (register / waitForDone / exit) stays in Node main.ts,
// which spawns this binary. No flags; configuration is by environment only
// (MAKEFUN_SHOP_TOKEN, MAKEFUN_TRIAL_NOW — see internal/store).
package main

import (
	"fmt"
	"net"
	"os"

	"forge/internal/server"
	"forge/internal/store"
)

func main() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] listen: %v\n", err)
		os.Exit(1)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		fmt.Fprintf(os.Stderr, "[entry] the server did not bind an ephemeral TCP port\n")
		os.Exit(1)
	}
	fmt.Printf("[entry] listening on port %d\n", addr.Port)
	s := store.New()
	// Serve blocks until the listener errors or the process is signalled
	// (SIGTERM default behaviour terminates the process; the Node launcher
	// SIGTERM+SIGKILLs this child on /done).
	if err := server.Serve(ln, s); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] serve: %v\n", err)
		os.Exit(1)
	}
}
