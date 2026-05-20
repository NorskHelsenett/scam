package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"unicode/utf8"

	"github.com/NorskHelsenett/scam/internal/collector"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// Populated at build time via -ldflags "-X main.version=... -X main.commit=...".
// See Dockerfile and .github/workflows/build-docker.yml.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	var (
		kubeconfig  string
		showVersion bool
	)
	defaultKC := ""
	if h := homedir.HomeDir(); h != "" {
		defaultKC = filepath.Join(h, ".kube", "config")
	}
	flag.StringVar(&kubeconfig, "kubeconfig", defaultKC, "path to kubeconfig (ignored in-cluster)")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()
	if showVersion {
		fmt.Printf("scam %s (%s)\n", version, commit)
		return
	}

	// CALLCENTER_URL is resolved after cluster identity detection so
	// the push loop can be started with the fully-configured logger.
	callcenterURL := strings.TrimSpace(os.Getenv("CALLCENTER_URL"))

	cfg, err := loadConfig(kubeconfig)
	if err != nil {
		collector.Log.Error("load kube config", "err", err)
		os.Exit(1)
	}
	cfg.QPS = 5
	cfg.Burst = 10

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		collector.Log.Error("build clientset", "err", err)
		os.Exit(1)
	}

	// ---- cluster identity --------------------------------------------------
	// Priority (per field):
	//   Name:        ROR Self() slug → CLUSTER_NAME env
	//   ID:          CLUSTER_ID env → ROR Self() slug → kube-system UID
	//   Environment: ROR /v1/clusters/<slug>.environment → ENVIRONMENT env
	//
	// fetchRorIdentity only fires when ROR_API_ENDPOINT + an apikey
	// (literal env or k8s Secret) are resolvable; without them SCAM
	// lands on the env / kube-system chain unchanged.
	ror := fetchRorIdentity(clientset)

	var kubeSystemUID string
	if ns, err := clientset.CoreV1().Namespaces().Get(context.TODO(), "kube-system", metav1.GetOptions{}); err == nil {
		kubeSystemUID = string(ns.UID)
	}

	clusterName := resolveClusterName(ror.Slug)
	clusterID := resolveClusterID(ror.Slug, kubeSystemUID)
	environment := resolveEnvironment(ror.Environment)

	var clusterAttrs []any
	if clusterName != "" {
		clusterAttrs = append(clusterAttrs, "cluster", clusterName)
	}
	if clusterID != "" {
		clusterAttrs = append(clusterAttrs, "cluster_id", clusterID)
	}
	if environment != "" {
		clusterAttrs = append(clusterAttrs, "environment", environment)
	}
	clusterAttrs = append(clusterAttrs, "version", version, "commit", commit)
	collector.Log = collector.Log.With(clusterAttrs...)

	// ---- startup banner ----------------------------------------------------
	printBanner(clusterName, clusterID, environment, callcenterURL, version, commit)

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		collector.Log.Error("build dynamic client", "err", err)
		os.Exit(1)
	}
	discClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		collector.Log.Error("build discovery client", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// LineCapture + logger get set up here so every subsequent log line
	// (informer setup, cache sync, the initial snapshot) is buffered for
	// push. PushLoop itself starts later — once podInf exists — so the
	// ACK-mismatch trigger has a snapshot target to fire.
	var pushCapture *collector.LineCapture
	var pushEndpoint string
	if callcenterURL != "" {
		pushCapture = &collector.LineCapture{Stdout: os.Stdout}
		collector.Log = slog.New(slog.NewJSONHandler(io.Writer(pushCapture), &slog.HandlerOptions{Level: slog.LevelInfo}))
		collector.Log = collector.Log.With(clusterAttrs...)
		base := strings.TrimRight(callcenterURL, "/")
		pushEndpoint = base + "/api/scam/callcenter"
		// Keep the server's session alive on quiet clusters — without
		// heartbeats, last_push_at only advances when we have actual
		// events to push, so a stable cluster falls out of the live
		// window and disappears from the UI.
		go collector.HeartbeatLoop(ctx, base+"/api/scam/heartbeat", clusterID)
	}

	var synced atomic.Bool

	// ---- typed informers ------------------------------------------------
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 0)

	podInf := factory.Core().V1().Pods()
	_ = podInf.Informer().SetTransform(collector.TrimPod)
	collector.OnEvents(podInf.Informer(), &synced,
		func(event string, newP, oldP *corev1.Pod) { collector.EmitPod(event, newP, oldP) },
		collector.EmitPodDelete,
	)

	// ReplicaSet informer — used only as a lister so PodOwner() can resolve
	// the ReplicaSet → Deployment owner chain. No event handlers needed.
	rsInf := factory.Apps().V1().ReplicaSets()
	_ = rsInf.Informer().SetTransform(collector.TrimReplicaSet)
	collector.Refs.ReplicaSets = rsInf

	svcInf := factory.Core().V1().Services()
	_ = svcInf.Informer().SetTransform(collector.TrimService)
	collector.OnEvents(svcInf.Informer(), &synced,
		func(event string, s, _ *corev1.Service) { collector.EmitService(event, s) },
		collector.EmitServiceDelete,
	)

	esInf := factory.Discovery().V1().EndpointSlices()
	_ = esInf.Informer().SetTransform(collector.TrimEndpointSlice)
	collector.OnEvents(esInf.Informer(), &synced,
		func(event string, es, _ *discoveryv1.EndpointSlice) { collector.EmitEndpointSlice(event, es) },
		collector.EmitEndpointSliceDelete,
	)

	ingInf := factory.Networking().V1().Ingresses()
	_ = ingInf.Informer().SetTransform(collector.TrimIngress)
	_ = ingInf.Informer().AddIndexers(cache.Indexers{collector.BackendIndexName: collector.IngressBackendKeys})
	collector.OnEvents(ingInf.Informer(), &synced,
		func(event string, i, _ *networkingv1.Ingress) {
			collector.EmitIngress(event, i)
			collector.RefreshBackendServices(collector.IngressBackends(i))
		},
		collector.EmitIngressDelete,
	)

	icInf := factory.Networking().V1().IngressClasses()
	_ = icInf.Informer().SetTransform(collector.TrimMeta)
	collector.OnEvents(icInf.Informer(), &synced,
		func(event string, ic, _ *networkingv1.IngressClass) { collector.EmitIngressClass(event, ic) },
		collector.EmitIngressClassDelete,
	)

	// ---- Gateway API + Traefik via dynamic informers ---
	var dynFactory dynamicinformer.DynamicSharedInformerFactory

	gwGVRs := collector.DiscoverGatewayAPI(discClient)
	gwInformers := map[string]cache.SharedIndexInformer{}
	trGVRs := collector.DiscoverTraefik(discClient)
	trInformers := map[string]cache.SharedIndexInformer{}

	if len(gwGVRs) > 0 || len(trGVRs) > 0 {
		dynFactory = dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 0)
	}
	if len(gwGVRs) > 0 {
		for _, gvr := range gwGVRs {
			inf := dynFactory.ForResource(gvr).Informer()
			_ = inf.SetTransform(collector.TrimUnstructured)
			if collector.IsRouteGVR(gvr) {
				_ = inf.AddIndexers(cache.Indexers{collector.BackendIndexName: collector.RouteBackendKeys})
			}
			collector.OnEvents(inf, &synced,
				func(event string, u, _ *unstructured.Unstructured) {
					collector.EmitGatewayAPI(event, gvr, u)
					if collector.IsRouteGVR(gvr) {
						collector.RefreshBackendServices(collector.RouteBackends(u))
					}
				},
				func(u *unstructured.Unstructured) { collector.EmitGatewayAPIDelete(gvr, u) },
			)
			gwInformers[gvr.String()] = inf
		}
		collector.Refs.GWInformers = gwInformers
		collector.Log.Info("gateway API detected", "resources", collector.GvrStrings(gwGVRs))
	} else {
		collector.Log.Info("gateway API CRDs not installed; skipping")
	}
	if len(trGVRs) > 0 {
		for _, gvr := range trGVRs {
			inf := dynFactory.ForResource(gvr).Informer()
			_ = inf.SetTransform(collector.TrimUnstructured)
			_ = inf.AddIndexers(cache.Indexers{collector.BackendIndexName: collector.TraefikBackendKeys})
			collector.OnEvents(inf, &synced,
				func(event string, u, _ *unstructured.Unstructured) {
					collector.EmitTraefik(event, gvr, u)
					collector.RefreshBackendServices(collector.TraefikBackends(u))
				},
				func(u *unstructured.Unstructured) { collector.EmitTraefikDelete(gvr, u) },
			)
			trInformers[gvr.String()] = inf
		}
		collector.Refs.TRInformers = trInformers
		collector.Log.Info("traefik CRDs detected", "resources", collector.GvrStrings(trGVRs))
	} else {
		collector.Log.Info("traefik CRDs not installed; skipping")
	}

	// ---- start + wait for sync -----------------------------------------
	factory.Start(ctx.Done())
	if dynFactory != nil {
		dynFactory.Start(ctx.Done())
	}

	collector.Refs.Services = svcInf
	collector.Refs.Ingresses = ingInf

	collector.Log.Info("waiting for cache sync")
	syncs := []cache.InformerSynced{
		podInf.Informer().HasSynced,
		rsInf.Informer().HasSynced,
		svcInf.Informer().HasSynced,
		esInf.Informer().HasSynced,
		ingInf.Informer().HasSynced,
		icInf.Informer().HasSynced,
	}
	for _, inf := range gwInformers {
		syncs = append(syncs, inf.HasSynced)
	}
	for _, inf := range trInformers {
		syncs = append(syncs, inf.HasSynced)
	}
	if !cache.WaitForCacheSync(ctx.Done(), syncs...) {
		collector.Log.Error("cache sync aborted")
		os.Exit(1)
	}

	// ---- initial full-state snapshot -----------------------------------
	// All tracked kinds emitted together inside a SNAPSHOT_BEGIN/_END
	// envelope so SPAM can tombstone whatever lingered from before this
	// process started. SnapshotLoop fires the periodic reconcile.
	rs := collector.SnapshotResources{
		Pods:           podInf,
		Services:       svcInf,
		EndpointSlices: esInf,
		Ingresses:      ingInf,
		IngressClasses: icInf,
		GatewayAPI:     collector.BuildDynamicSnapshots(gwGVRs, gwInformers),
		Traefik:        collector.BuildDynamicSnapshots(trGVRs, trInformers),
	}
	collector.EmitFullSnapshot(rs, "init")

	synced.Store(true)
	collector.Log.Info("streaming events")
	go collector.SnapshotLoop(ctx, rs)

	// PushLoop starts here so the ACK-mismatch trigger can fire a
	// reconcile snapshot against the (now-synced) informer caches —
	// covers every tracked kind via EmitFullSnapshot, not just
	// Containers.
	if pushCapture != nil {
		go collector.PushLoop(ctx, pushEndpoint, pushCapture, func() {
			collector.EmitFullSnapshot(rs, "reconcile")
		})
	}

	<-ctx.Done()
	collector.Log.Info("shutdown")
}

func printBanner(clusterName, clusterID, environment, callcenter, version, commit string) {
	title := "SCAM \u2014 SPAM Cluster Agent Metadata"
	lines := []string{
		fmt.Sprintf("version:     %s (%s)", version, commit),
		fmt.Sprintf("cluster:     %s", clusterName),
		fmt.Sprintf("cluster_id:  %s", clusterID),
		fmt.Sprintf("environment: %s", environment),
		fmt.Sprintf("callcenter:  %s", callcenter),
	}

	maxW := utf8.RuneCountInString(title)
	for _, l := range lines {
		if w := utf8.RuneCountInString(l); w > maxW {
			maxW = w
		}
	}

	hr := strings.Repeat("\u2500", maxW+2)
	pad := func(s string) string {
		return s + strings.Repeat(" ", maxW-utf8.RuneCountInString(s))
	}

	fmt.Fprintf(os.Stderr, "\u250c%s\u2510\n", hr)
	fmt.Fprintf(os.Stderr, "\u2502 %s \u2502\n", pad(title))
	fmt.Fprintf(os.Stderr, "\u251c%s\u2524\n", hr)
	fmt.Fprintf(os.Stderr, "\u2502 %s \u2502\n", pad(""))
	for _, l := range lines {
		fmt.Fprintf(os.Stderr, "\u2502 %s \u2502\n", pad(l))
	}
	fmt.Fprintf(os.Stderr, "\u2514%s\u2518\n", hr)
}

// resolveClusterName priority:
//  1. ROR API Self() (when ROR_API_ENDPOINT + ROR_API_KEY are set)
//  2. CLUSTER_NAME env var (set in helm chart) — may be empty if unset
func resolveClusterName(rorSelfName string) string {
	if rorSelfName != "" {
		return rorSelfName
	}
	return os.Getenv("CLUSTER_NAME")
}

// resolveClusterID priority:
//  1. CLUSTER_ID env var (explicit override)
//  2. ROR API Self() — the ROR-canonical cluster slug (e.g. t-tek-003-n2ua),
//     used by SPAM/ROR to identify the cluster across systems
//  3. kube-system namespace UID (stable local fingerprint, last resort)
func resolveClusterID(rorSelfName, kubeSystemUID string) string {
	if v := strings.TrimSpace(os.Getenv("CLUSTER_ID")); v != "" {
		return v
	}
	if rorSelfName != "" {
		return rorSelfName
	}
	return kubeSystemUID
}

// resolveEnvironment priority:
//  1. ROR cluster object's environment field (when fetched successfully)
//  2. ENVIRONMENT env var — may be empty if unset
func resolveEnvironment(rorEnv string) string {
	if rorEnv != "" {
		return rorEnv
	}
	return os.Getenv("ENVIRONMENT")
}

func loadConfig(kubeconfig string) (*rest.Config, error) {
	if c, err := rest.InClusterConfig(); err == nil {
		return c, nil
	}
	if kubeconfig == "" {
		return nil, fmt.Errorf("no in-cluster config and no kubeconfig given")
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}
