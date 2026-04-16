// scam (SPAM Cluster Agent Metadata) watches cluster metadata (Pods + their container images and
// the exposure chain: Service, Ingress, IngressClass, EndpointSlice, and
// Gateway API resources when installed) and emits per-object JSON records
// on stdout for later ingest.
//
// It's a dumb collector. It does not join, does not classify local-vs-public
// IPs, and does not resolve OCI image labels. Those decisions live in the
// ingesting system so rule changes don't require redeploying N operators.
//
// Memory is kept low by:
//   - disabling resync (no periodic full relists)
//   - per-informer transform functions that strip unread fields from cached
//     objects (annotations, managed fields, volumes, env, probes, ...)
//   - not keeping any parallel cache of state in this process
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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

var (
	log     = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cluster string // from CLUSTER_NAME env var; stamped on every emitted record
)

func main() {
	var kubeconfig string
	defaultKC := ""
	if h := homedir.HomeDir(); h != "" {
		defaultKC = filepath.Join(h, ".kube", "config")
	}
	flag.StringVar(&kubeconfig, "kubeconfig", defaultKC, "path to kubeconfig (ignored in-cluster)")
	flag.Parse()

	cluster = os.Getenv("CLUSTER_NAME")
	if cluster == "" {
		log.Warn("CLUSTER_NAME is empty; emitted records will carry cluster=\"\"")
	}

	printBanner()

	cfg, err := loadConfig(kubeconfig)
	if err != nil {
		log.Error("load kube config", "err", err)
		os.Exit(1)
	}
	cfg.QPS = 5
	cfg.Burst = 10

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Error("build clientset", "err", err)
		os.Exit(1)
	}
	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Error("build dynamic client", "err", err)
		os.Exit(1)
	}
	discClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		log.Error("build discovery client", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var synced atomic.Bool

	// ---- typed informers ------------------------------------------------
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 0)

	podInf := factory.Core().V1().Pods()
	_ = podInf.Informer().SetTransform(trimPod)
	onEvents(podInf.Informer(), &synced,
		func(event string, newP, oldP *corev1.Pod) { emitPod(event, newP, oldP) },
		emitPodDelete,
	)

	svcInf := factory.Core().V1().Services()
	_ = svcInf.Informer().SetTransform(trimService)
	onEvents(svcInf.Informer(), &synced,
		func(event string, s, _ *corev1.Service) { emitService(event, s) },
		emitServiceDelete,
	)

	ingInf := factory.Networking().V1().Ingresses()
	_ = ingInf.Informer().SetTransform(trimIngress)
	_ = ingInf.Informer().AddIndexers(cache.Indexers{backendIndexName: ingressBackendKeys})
	onEvents(ingInf.Informer(), &synced,
		func(event string, i, _ *networkingv1.Ingress) {
			emitIngress(event, i)
			refreshBackendServices(ingressBackends(i))
		},
		emitIngressDelete,
	)

	icInf := factory.Networking().V1().IngressClasses()
	_ = icInf.Informer().SetTransform(trimMeta)
	onEvents(icInf.Informer(), &synced,
		func(event string, ic, _ *networkingv1.IngressClass) { emitIngressClass(event, ic) },
		emitIngressClassDelete,
	)

	// ---- Gateway API + Traefik via dynamic informers, each only if CRDs are installed ---
	var dynFactory dynamicinformer.DynamicSharedInformerFactory

	gwGVRs := discoverGatewayAPI(discClient)
	gwInformers := map[string]cache.SharedIndexInformer{}
	trGVRs := discoverTraefik(discClient)
	trInformers := map[string]cache.SharedIndexInformer{}

	if len(gwGVRs) > 0 || len(trGVRs) > 0 {
		dynFactory = dynamicinformer.NewDynamicSharedInformerFactory(dynClient, 0)
	}
	if len(gwGVRs) > 0 {
		for _, gvr := range gwGVRs {
			inf := dynFactory.ForResource(gvr).Informer()
			_ = inf.SetTransform(trimUnstructured)
			if isRouteGVR(gvr) {
				_ = inf.AddIndexers(cache.Indexers{backendIndexName: routeBackendKeys})
			}
			onEvents(inf, &synced,
				func(event string, u, _ *unstructured.Unstructured) {
					emitGatewayAPI(event, gvr, u)
					if isRouteGVR(gvr) {
						refreshBackendServices(routeBackends(u))
					}
				},
				func(u *unstructured.Unstructured) { emitGatewayAPIDelete(gvr, u) },
			)
			gwInformers[gvr.String()] = inf
		}
		refs.gwInformers = gwInformers
		log.Info("gateway API detected", "resources", gvrStrings(gwGVRs))
	} else {
		log.Info("gateway API CRDs not installed; skipping")
	}
	if len(trGVRs) > 0 {
		for _, gvr := range trGVRs {
			inf := dynFactory.ForResource(gvr).Informer()
			_ = inf.SetTransform(trimUnstructured)
			_ = inf.AddIndexers(cache.Indexers{backendIndexName: traefikBackendKeys})
			onEvents(inf, &synced,
				func(event string, u, _ *unstructured.Unstructured) {
					emitTraefik(event, gvr, u)
					refreshBackendServices(traefikBackends(u))
				},
				func(u *unstructured.Unstructured) { emitTraefikDelete(gvr, u) },
			)
			trInformers[gvr.String()] = inf
		}
		refs.trInformers = trInformers
		log.Info("traefik CRDs detected", "resources", gvrStrings(trGVRs))
	} else {
		log.Info("traefik CRDs not installed; skipping")
	}

	// ---- start + wait for sync -----------------------------------------
	factory.Start(ctx.Done())
	if dynFactory != nil {
		dynFactory.Start(ctx.Done())
	}

	// Stash informers we need to cross-reference (exposure chain lookups) so
	// emit functions can check "is this Service/Pod reachable from outside?".
	refs.services = svcInf
	refs.ingresses = ingInf

	log.Info("waiting for cache sync")
	syncs := []cache.InformerSynced{
		podInf.Informer().HasSynced,
		svcInf.Informer().HasSynced,
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
		log.Error("cache sync aborted")
		os.Exit(1)
	}

	// ---- initial sorted snapshot per kind ------------------------------
	dumpPods(podInf)
	// Route-bearing resources before Services so exposure-reference lookups
	// find routes in-cache when we filter ClusterIP services.
	for _, gvr := range gwGVRs {
		dumpGatewayAPI(gvr, gwInformers[gvr.String()])
	}
	for _, gvr := range trGVRs {
		dumpTraefik(gvr, trInformers[gvr.String()])
	}
	dumpIngresses(ingInf)
	dumpIngressClasses(icInf)
	dumpServices(svcInf)

	synced.Store(true)
	log.Info("streaming events")

	<-ctx.Done()
	log.Info("shutdown")
}

func printBanner() {
	clusterID := os.Getenv("CLUSTER_ID")
	environment := os.Getenv("ENVIRONMENT")
	callcenter := os.Getenv("CALLCENTER")

	title := "SCAM \u2014 SPAM Cluster Agent Metadata"
	lines := []string{
		fmt.Sprintf("cluster:     %s", cluster),
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

// loadConfig tries in-cluster first, then falls back to kubeconfig.
func loadConfig(kubeconfig string) (*rest.Config, error) {
	if c, err := rest.InClusterConfig(); err == nil {
		return c, nil
	}
	if kubeconfig == "" {
		return nil, fmt.Errorf("no in-cluster config and no kubeconfig given")
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}
