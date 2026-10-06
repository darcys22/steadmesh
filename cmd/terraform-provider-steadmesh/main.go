// Command terraform-provider-steadmesh serves the steadmesh Terraform provider.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/darcys22/steadmesh/provider"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	debug := flag.Bool("debug", false, "run with support for debuggers such as delve")
	flag.Parse()
	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address:         provider.Address,
		Debug:           *debug,
		ProtocolVersion: 6,
	})
	if err != nil {
		log.Fatal(err)
	}
}
