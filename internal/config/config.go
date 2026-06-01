// Package config defines shared types used by both the provider and state store packages.
package config

import "net/http"

// ProviderData holds provider-level configuration forwarded to state stores
// via provider.ConfigureResponse.StateStoreData → statestore.InitializeRequest.ProviderData.
type ProviderData struct {
	// Insecure disables TLS certificate verification when true.
	Insecure bool
	// CAFile is the path to a PEM-encoded CA bundle; empty means system CAs.
	CAFile string
	// HTTPClient is pre-configured with the TLS settings above.
	HTTPClient *http.Client
}
