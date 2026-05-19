package main

import (
	"os"
	"strings"

	"github.com/NorskHelsenett/ror/pkg/clients/rorclient"
	"github.com/NorskHelsenett/ror/pkg/clients/rorclient/v2/transports/resttransport"
	"github.com/NorskHelsenett/ror/pkg/clients/rorclient/v2/transports/resttransport/httpauthprovider"
	"github.com/NorskHelsenett/ror/pkg/clients/rorclient/v2/transports/resttransport/httpclient"
	identitymodels "github.com/NorskHelsenett/ror/pkg/models/identity"
	"github.com/NorskHelsenett/ror/pkg/config/rorversion"

	"github.com/NorskHelsenett/scam/internal/collector"
)

// fetchRorSelfName asks ROR for the cluster identity bound to the
// mounted apikey. Returns "" (not an error) when ROR_API_ENDPOINT or
// ROR_API_KEY are unset, or when the call/assertion fails — callers
// continue down the env/kube-system fallback chain.
func fetchRorSelfName() string {
	endpoint := strings.TrimSpace(os.Getenv("ROR_API_ENDPOINT"))
	apikey := strings.TrimSpace(os.Getenv("ROR_API_KEY"))
	if endpoint == "" || apikey == "" {
		return ""
	}

	auth := httpauthprovider.NewAuthProvider(httpauthprovider.AuthPoviderTypeAPIKey, apikey)
	transport := resttransport.NewRorHttpTransport(&httpclient.HttpTransportClientConfig{
		BaseURL:      endpoint,
		AuthProvider: auth,
		Version:      rorversion.GetRorVersion(),
		Role:         "scam",
	})
	cli := rorclient.NewRorClient(transport)

	self, err := cli.V2().Self().Get()
	if err != nil {
		collector.Log.Warn("ror self lookup failed", "err", err)
		return ""
	}
	if self.Type != identitymodels.IdentityTypeCluster {
		collector.Log.Warn("ror apikey is not bound to a cluster identity", "type", self.Type)
		return ""
	}
	return strings.TrimSpace(self.User.Name)
}
