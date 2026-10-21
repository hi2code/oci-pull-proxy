// The root entry point supports `go run .`; the container uses cmd/oci-pull-proxy.
package main

import (
	"example.com/oci-pull-proxy/internal/app"
	"os"
)

func main() { os.Exit(app.Main(os.Args[1:], os.Stdout, os.Stderr)) }
