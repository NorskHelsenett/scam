package main

import (
	"context"
	"os"
	"strings"

	"github.com/NorskHelsenett/ror/pkg/clients/rorclient"
	"github.com/NorskHelsenett/ror/pkg/clients/rorclient/v2/transports/resttransport"
	"github.com/NorskHelsenett/ror/pkg/clients/rorclient/v2/transports/resttransport/httpauthprovider"
	"github.com/NorskHelsenett/ror/pkg/clients/rorclient/v2/transports/resttransport/httpclient"
	"github.com/NorskHelsenett/ror/pkg/config/rorversion"
	identitymodels "github.com/NorskHelsenett/ror/pkg/models/identity"

	"github.com/NorskHelsenett/scam/internal/collector"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// fetchRorSelfName asks ROR for the cluster identity bound to the
// mounted apikey. Returns "" (not an error) when the endpoint or the
// apikey can't be resolved — callers continue down the env/kube-system
// fallback chain.
//
// Apikey resolution order:
//  1. ROR_API_KEY env var (literal value)
//  2. K8s Secret pointed at by ROR_API_KEY_SECRET_NAMESPACE +
//     ROR_API_KEY_SECRET_NAME + ROR_API_KEY_SECRET_KEY (read via the
//     in-cluster client — RBAC-gated)
func fetchRorSelfName(clientset *kubernetes.Clientset) string {
	endpoint := strings.TrimSpace(os.Getenv("ROR_API_ENDPOINT"))
	if endpoint == "" {
		collector.Log.Info("ror: skipping self lookup (ROR_API_ENDPOINT unset)")
		return ""
	}
	collector.Log.Info("ror: self lookup begin", "endpoint", endpoint)

	apikey, source := resolveRorApiKey(clientset)
	if apikey == "" {
		collector.Log.Warn("ror: apikey not resolved; falling back to env/UID chain")
		return ""
	}
	collector.Log.Info("ror: apikey resolved", "source", source, "len", len(apikey))

	auth := httpauthprovider.NewAuthProvider(httpauthprovider.AuthPoviderTypeAPIKey, apikey)
	transport := resttransport.NewRorHttpTransport(&httpclient.HttpTransportClientConfig{
		BaseURL:      endpoint,
		AuthProvider: auth,
		Version:      rorversion.GetRorVersion(),
		Role:         "scam",
	})
	cli := rorclient.NewRorClient(transport)

	self, err := cli.V2().Self().Get(context.Background())
	if err != nil {
		collector.Log.Warn("ror self lookup failed", "err", err)
		return ""
	}
	collector.Log.Info("ror: self lookup ok", "type", self.Type, "name", self.User.Name)
	if self.Type != identitymodels.IdentityTypeCluster {
		collector.Log.Warn("ror apikey is not bound to a cluster identity", "type", self.Type)
		return ""
	}
	return strings.TrimSpace(self.User.Name)
}

// resolveRorApiKey returns the apikey value and a short source label
// for logging ("env" or "secret:<ns>/<name>"). The apikey value itself
// is never logged.
func resolveRorApiKey(clientset *kubernetes.Clientset) (string, string) {
	if v := strings.TrimSpace(os.Getenv("ROR_API_KEY")); v != "" {
		return v, "env:ROR_API_KEY"
	}
	ns := strings.TrimSpace(os.Getenv("ROR_API_KEY_SECRET_NAMESPACE"))
	name := strings.TrimSpace(os.Getenv("ROR_API_KEY_SECRET_NAME"))
	key := strings.TrimSpace(os.Getenv("ROR_API_KEY_SECRET_KEY"))
	if ns == "" || name == "" || key == "" {
		collector.Log.Warn("ror: apikey env vars incomplete",
			"namespace_set", ns != "", "name_set", name != "", "key_set", key != "")
		return "", ""
	}
	collector.Log.Info("ror: reading apikey secret", "ns", ns, "name", name, "key", key)
	secret, err := clientset.CoreV1().Secrets(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		collector.Log.Warn("ror apikey secret read failed", "ns", ns, "name", name, "err", err)
		return "", ""
	}
	raw, ok := secret.Data[key]
	if !ok || len(raw) == 0 {
		collector.Log.Warn("ror apikey secret has no value at key",
			"ns", ns, "name", name, "key", key, "keys_present", secretKeys(secret.Data))
		return "", ""
	}
	return strings.TrimSpace(string(raw)), "secret:" + ns + "/" + name
}

func secretKeys(data map[string][]byte) []string {
	out := make([]string, 0, len(data))
	for k := range data {
		out = append(out, k)
	}
	return out
}
