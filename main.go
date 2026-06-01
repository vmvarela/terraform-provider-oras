// Package main is the entry point for the terraform-provider-orastate provider.
// It serves the OCI state store provider using the Terraform Plugin Framework.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	internalprovider "github.com/vmvarela/terraform-provider-orastate/internal/provider"
)

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers")
	flag.Parse()

	err := providerserver.Serve(context.Background(), internalprovider.New(), providerserver.ServeOpts{
		Address: "registry.terraform.io/vmvarela/orastate",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
