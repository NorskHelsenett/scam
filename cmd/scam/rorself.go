package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
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

const (
	envRorAPIEndpoint      = "ROR_API_ENDPOINT"
	envRorAPIKey           = "ROR_API_KEY"
	envRorAPIKeySecretNS   = "ROR_API_KEY_SECRET_NAMESPACE"
	envRorAPIKeySecretName = "ROR_API_KEY_SECRET_NAME"
	envRorAPIKeySecretKey  = "ROR_API_KEY_SECRET_KEY"

	rorLookupTimeout = 10 * time.Second

	defaultRorRefreshInterval = 1 * time.Hour
	// rorRetryInterval is the tighter cadence used while the identity is
	// still unresolved — e.g. the agent started before the cluster was
	// registered in ROR or before the apikey Secret existed.
	rorRetryInterval = 10 * time.Minute
)

// httpClient is reused for both ROR HTTP calls so a hung TCP connect
// can't outlive the per-call context grace.
var httpClient = &http.Client{Timeout: rorLookupTimeout}

// rorIdentity is what ROR knows about the cluster this agent runs in.
// All fields are best-effort; consumers must tolerate empty values.
//
// None of these fields are the cluster's primary identity — that is the
// kube-system namespace UID, resolved in main. The ROR fields are ACL/
// display metadata emitted under ror_metadata.
type rorIdentity struct {
	Slug        string // V2 Self().User.Name — ROR's binding for this cluster (emitted as ror_metadata.cluster_id)
	Name        string // /v1/clusters/<slug>.clusterName — human-friendly display name
	Environment string // /v1/clusters/<slug>.environment
}

// rorEndpoint returns the configured ROR API endpoint (empty disables ROR lookup).
func rorEndpoint() string { return strings.TrimSpace(os.Getenv(envRorAPIEndpoint)) }

// clusterAttrSet builds the identity attribute set stamped on every
// emitted record (via collector.SetClusterAttrs). ror_metadata is a
// nested group, emitted only when ROR lookup succeeded. SPAM joins on
// top-level cluster_id (kube-system UID) regardless, and uses
// ror_metadata.cluster_id to map the cluster onto ROR's ACL/display
// when present.
func clusterAttrSet(clusterName, clusterID, environment string, ror rorIdentity) []slog.Attr {
	var attrs []slog.Attr
	if clusterName != "" {
		attrs = append(attrs, slog.String("cluster", clusterName))
	}
	if clusterID != "" {
		attrs = append(attrs, slog.String("cluster_id", clusterID))
	}
	if environment != "" {
		attrs = append(attrs, slog.String("environment", environment))
	}
	if ror.Slug != "" {
		attrs = append(attrs, slog.Group("ror_metadata",
			"cluster_id", ror.Slug,
			"cluster_name", ror.Name,
			"env", ror.Environment,
		))
	}
	attrs = append(attrs, slog.String("version", version), slog.String("commit", commit))
	return attrs
}

// rorIdentityLoop periodically re-resolves the cluster's ROR identity
// and swaps the record attrs when it changes. A one-shot boot lookup
// misses two real cases:
//   - the agent started before the cluster was registered in ROR (or
//     before the apikey Secret existed), so the binding appears later;
//   - ROR renames the cluster's identity (e.g. a slug → UUID
//     migration), which every running agent would otherwise keep
//     pushing stale until restarted.
//
// The apikey is re-read from env/Secret each round so a Secret created
// or rotated after boot is picked up too. A transient lookup failure
// keeps the current identity — attrs only change on a successful,
// different resolution. Cadence is ROR_REFRESH_INTERVAL (default 1h,
// "off"/"0" disables), tightened to rorRetryInterval while unresolved.
func rorIdentityLoop(ctx context.Context, clientset *kubernetes.Clientset, clusterID string, current rorIdentity) {
	endpoint := rorEndpoint()
	if endpoint == "" {
		return
	}
	interval := resolveRorRefreshInterval()
	if interval <= 0 {
		collector.Log.Info("ror refresh: disabled")
		return
	}
	next := func() time.Duration {
		if current == (rorIdentity{}) {
			return min(rorRetryInterval, interval)
		}
		return interval
	}
	collector.Log.Info("ror refresh: scheduled", "interval", interval, "next", next())

	timer := time.NewTimer(next())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		if apikey := resolveRorApiKey(clientset); apikey != "" {
			if id := fetchRorIdentity(endpoint, apikey); id != (rorIdentity{}) && id != current {
				name := resolveClusterName(id.Name)
				env := resolveEnvironment(id.Environment)
				collector.SetClusterAttrs(clusterAttrSet(name, clusterID, env, id))
				collector.Log.Info("ror identity updated",
					"ror_cluster_id", id.Slug,
					"previous_ror_cluster_id", current.Slug,
					"cluster", name,
					"environment", env)
				current = id
			}
		}
		timer.Reset(next())
	}
}

func resolveRorRefreshInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("ROR_REFRESH_INTERVAL"))
	if raw == "" {
		return defaultRorRefreshInterval
	}
	if strings.EqualFold(raw, "off") || strings.EqualFold(raw, "disabled") || raw == "0" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < time.Minute {
		collector.Log.Warn("ror refresh: invalid ROR_REFRESH_INTERVAL, using default",
			"value", raw, "default", defaultRorRefreshInterval)
		return defaultRorRefreshInterval
	}
	return d
}

// fetchRorIdentity resolves the cluster's ROR identity in two hops:
// V2 Self() for the slug (and the Type=Cluster assertion), then
// /v1/clusters/<slug> for display name + environment (Self()'s
// response shape doesn't carry either). Returns a zero value when
// either call fails.
func fetchRorIdentity(endpoint, apikey string) rorIdentity {
	slug := rorSelfLookup(endpoint, apikey)
	if slug == "" {
		return rorIdentity{}
	}
	name, env := rorClusterLookup(endpoint, apikey, slug)
	return rorIdentity{Slug: slug, Name: name, Environment: env}
}

func rorSelfLookup(endpoint, apikey string) string {
	auth := httpauthprovider.NewAuthProvider(httpauthprovider.AuthPoviderTypeAPIKey, apikey)
	transport := resttransport.NewRorHttpTransport(&httpclient.HttpTransportClientConfig{
		BaseURL:      endpoint,
		AuthProvider: auth,
		Version:      rorversion.GetRorVersion(),
		Role:         "scam",
	})
	cli := rorclient.NewRorClient(transport)

	ctx, cancel := context.WithTimeout(context.Background(), rorLookupTimeout)
	defer cancel()
	self, err := cli.V2().Self().Get(ctx)
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

// rorClusterLookup hand-rolls the /v1/clusters/<slug> call instead of
// going through rorclient.V2().Resources() — the typed SDK path
// needs a GroupVersionKind we'd have to guess at, and we only want
// two fields off the response.
func rorClusterLookup(endpoint, apikey, slug string) (name, env string) {
	url := strings.TrimRight(endpoint, "/") + "/v1/clusters/" + slug
	ctx, cancel := context.WithTimeout(context.Background(), rorLookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		collector.Log.Warn("ror cluster fetch build request failed", "err", err)
		return "", ""
	}
	req.Header.Set("X-API-KEY", apikey)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		collector.Log.Warn("ror cluster fetch failed", "url", url, "err", err)
		return "", ""
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode >= 300 {
		collector.Log.Warn("ror cluster fetch non-2xx",
			"url", url, "status", resp.StatusCode, "body", truncate(string(body), 512))
		return "", ""
	}
	var out struct {
		ClusterName string `json:"clusterName"`
		Environment string `json:"environment"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		collector.Log.Warn("ror cluster decode failed", "err", err, "body", truncate(string(body), 256))
		return "", ""
	}
	return strings.TrimSpace(out.ClusterName), strings.TrimSpace(out.Environment)
}

// resolveRorApiKey returns the apikey value, never logging the value
// itself. Order: ROR_API_KEY env var (literal) → in-cluster Secret
// pointed at by ROR_API_KEY_SECRET_NAMESPACE + _NAME + _KEY.
func resolveRorApiKey(clientset *kubernetes.Clientset) string {
	if v := strings.TrimSpace(os.Getenv(envRorAPIKey)); v != "" {
		return v
	}
	ns := strings.TrimSpace(os.Getenv(envRorAPIKeySecretNS))
	name := strings.TrimSpace(os.Getenv(envRorAPIKeySecretName))
	key := strings.TrimSpace(os.Getenv(envRorAPIKeySecretKey))
	if ns == "" || name == "" || key == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), rorLookupTimeout)
	defer cancel()
	secret, err := clientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		collector.Log.Warn("ror apikey secret read failed", "ns", ns, "name", name, "err", err)
		return ""
	}
	raw, ok := secret.Data[key]
	if !ok || len(raw) == 0 {
		collector.Log.Warn("ror apikey secret has no value at key",
			"ns", ns, "name", name, "key", key, "keys_present", secretKeys(secret.Data))
		return ""
	}
	return strings.TrimSpace(string(raw))
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
