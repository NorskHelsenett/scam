package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

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

// rorIdentity is what ROR knows about the cluster this agent runs in.
// All fields are best-effort; consumers must tolerate empty values.
type rorIdentity struct {
	Slug        string // V2 Self() User.Name — the ROR-canonical cluster identifier
	Environment string // /v1/clusters/<slug> environment field
}

// fetchRorIdentity resolves the cluster's ROR identity in two hops:
// V2 Self() for the slug + Type assertion, then a direct GET to
// /v1/clusters/<slug> for the environment (the V2 Self response
// shape doesn't carry environment). Returns a zero value when ROR
// can't be reached or the apikey can't be resolved — callers
// continue down the env/UID fallback chain.
func fetchRorIdentity(clientset *kubernetes.Clientset) rorIdentity {
	var zero rorIdentity
	endpoint := strings.TrimSpace(os.Getenv("ROR_API_ENDPOINT"))
	if endpoint == "" {
		collector.Log.Info("ror: skipping self lookup (ROR_API_ENDPOINT unset)")
		return zero
	}
	apikey, source := resolveRorApiKey(clientset)
	if apikey == "" {
		collector.Log.Warn("ror: apikey not resolved; falling back to env/UID chain")
		return zero
	}
	collector.Log.Info("ror: apikey resolved", "source", source, "len", len(apikey))

	slug := rorSelfLookup(endpoint, apikey)
	if slug == "" {
		return zero
	}
	return rorIdentity{
		Slug:        slug,
		Environment: rorClusterLookup(endpoint, apikey, slug),
	}
}

func rorSelfLookup(endpoint, apikey string) string {
	collector.Log.Info("ror: self lookup begin", "endpoint", endpoint)
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

// rorClusterLookup hand-rolls the /v1/clusters/<slug> call instead of
// going through rorclient.V2().Resources() — the typed SDK path
// requires a GroupVersionKind we'd have to guess at, and we only want
// two fields off the response.
func rorClusterLookup(endpoint, apikey, slug string) string {
	url := strings.TrimRight(endpoint, "/") + "/v1/clusters/" + slug
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		collector.Log.Warn("ror cluster fetch build request failed", "err", err)
		return ""
	}
	req.Header.Set("X-API-KEY", apikey)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		collector.Log.Warn("ror cluster fetch failed", "url", url, "err", err)
		return ""
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode >= 300 {
		collector.Log.Warn("ror cluster fetch non-2xx",
			"url", url, "status", resp.StatusCode, "body", truncate(string(body), 512))
		return ""
	}
	var out struct {
		ClusterName string `json:"clusterName"`
		Environment string `json:"environment"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		collector.Log.Warn("ror cluster decode failed", "err", err, "body", truncate(string(body), 256))
		return ""
	}
	collector.Log.Info("ror: cluster fetched", "slug", slug, "name", out.ClusterName, "env", out.Environment)
	return strings.TrimSpace(out.Environment)
}

// resolveRorApiKey returns the apikey value and a short source label
// for logging ("env:..." or "secret:<ns>/<name>"). The apikey value
// itself is never logged.
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
