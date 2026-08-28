// Package providerdata holds shared types between provider and state store packages
// to avoid import cycles.
package providerdata

import "net/http"

// ProviderData holds provider-level configuration forwarded to state stores
// via ConfigureResponse.StateStoreData.
type ProviderData struct {
	Insecure   bool
	CAFile     string
	HTTPClient *http.Client
}