// Command credential-budget builds the CLIProxyAPI shared-library plugin.
//
// The real entry point is cliproxy_plugin_init in cabi.go, which only exists in
// the `cshared` build. This file keeps `go build ./...` and `go test ./...`
// working without cgo.
package main

func main() {}
