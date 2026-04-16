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

	clusterinterregator "github.com/NorskHelsenett/ror/pkg/kubernetes/interregators/clusterinterregator/v2"
	"github.com/NorskHelsenett/scam/internal/collector"

	corev1 "k8s.io/api/core/v1"
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

func main() {
	var kubeconfig string
	defaultKC := ""
	if h := homedir.HomeDir(); h != "" {
		defaultKC = filepath.Join(h, ".kube", "config")
	}
	flag.StringVar(&kubeconfig, "kubeconfig", defaultKC, "path to kubeconfig (ignored in-cluster)")
	flag.Parse()

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

	// ---- cluster identity (auto-detected from node metadata) ---------------
	ci := clusterinterregator.NewClusterInterregatorFromKubernetesClient(clientset)

	clusterID := ci.GetClusterId()
	if clusterID == "" || clusterID == "unknown-undefined" || clusterID == "unknown-cluster-id" {
		if ns, err := clientset.CoreV1().Namespaces().Get(context.TODO(), "kube-system", metav1.GetOptions{}); err == nil {
			clusterID = string(ns.UID)
		}
	}

	clusterName := ci.GetClusterName()
	if clusterName == "" || clusterName == "unknown-undefined" {
		clusterName = os.Getenv("CLUSTER_NAME")
	}

	environment := ci.GetEnvironment()
	if environment == "" || environment == "unknown-undefined" {
		environment = os.Getenv("ENVIRONMENT")
	}

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
	collector.Log = collector.Log.With(clusterAttrs...)

	// ---- startup banner ----------------------------------------------------
	printBanner(clusterName, clusterID, environment, callcenterURL)

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

	// Re-resolve capture for push loop now that logger is configured.
	if callcenterURL != "" {
		// Reconstruct capture from the logger's writer. Since we set it above,
		// just create a fresh one with the configured logger.
		capture := &collector.LineCapture{Stdout: os.Stdout}
		collector.Log = slog.New(slog.NewJSONHandler(io.Writer(capture), &slog.HandlerOptions{Level: slog.LevelInfo}))
		collector.Log = collector.Log.With(clusterAttrs...)
		endpoint := strings.TrimRight(callcenterURL, "/") + "/api/scam/callcenter"
		go collector.PushLoop(ctx, endpoint, capture)
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

	// ---- initial sorted snapshot per kind ------------------------------
	collector.DumpPods(podInf)
	for _, gvr := range gwGVRs {
		collector.DumpGatewayAPI(gvr, gwInformers[gvr.String()])
	}
	for _, gvr := range trGVRs {
		collector.DumpTraefik(gvr, trInformers[gvr.String()])
	}
	collector.DumpIngresses(ingInf)
	collector.DumpIngressClasses(icInf)
	collector.DumpServices(svcInf)

	synced.Store(true)
	collector.Log.Info("streaming events")

	<-ctx.Done()
	collector.Log.Info("shutdown")
}

func printBanner(clusterName, clusterID, environment, callcenter string) {
	title := "SCAM \u2014 SPAM Cluster Agent Metadata"
	lines := []string{
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

func loadConfig(kubeconfig string) (*rest.Config, error) {
	if c, err := rest.InClusterConfig(); err == nil {
		return c, nil
	}
	if kubeconfig == "" {
		return nil, fmt.Errorf("no in-cluster config and no kubeconfig given")
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}
