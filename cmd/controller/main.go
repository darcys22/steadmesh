// Command controller runs the AgentOrganization and AgentSeat reconcilers.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/controller"
	"github.com/darcys22/steadmesh/controller/platform"
	"github.com/darcys22/steadmesh/runtime/kube"
)

func main() {
	var (
		platformURL    = flag.String("platform-url", "http://steadmesh-platform.steadmesh-system.svc:8080", "platform service URL, propagated to seats as STEADMESH_PLATFORM_URL")
		cpNamespace    = flag.String("control-plane-namespace", "steadmesh-system", "namespace of the platform Pods seats may reach")
		pullPolicy     = flag.String("seat-image-pull-policy", string(corev1.PullIfNotPresent), "imagePullPolicy of seat and probe containers")
		netprobeImage  = flag.String("netprobe-image", "steadmesh/seat-fake:dev", "image providing `seat-runner netprobe` for the NetworkPolicy enforcement probe")
		netprobeDenied = flag.String("netprobe-denied-url", "https://kubernetes.default.svc:443/livez", "URL that must be unreachable from a seat when its NetworkPolicy is enforced")
		dnsNamespace   = flag.String("dns-namespace", "kube-system", "namespace of the cluster DNS service seats may query")
		egressURL      = flag.String("egress-url", "", "egress gateway URL (http://steadmesh-egress.<ns>.svc:3128); empty when the gateway is not enabled")
		tokenFile      = flag.String("token-file", platform.DefaultTokenFile, "controller ServiceAccount token presented to the platform internal API")
		metricsAddr    = flag.String("metrics-bind-address", ":8080", "metrics endpoint bind address (0 disables)")
		probeAddr      = flag.String("health-probe-bind-address", ":8081", "health probe bind address")
		leaderElect    = flag.Bool("leader-elect", true, "enable leader election")
		leaderNS       = flag.String("leader-election-namespace", "", "namespace of the leader election lease (default: in-cluster namespace)")
	)
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	log := ctrl.Log.WithName("setup")

	switch corev1.PullPolicy(*pullPolicy) {
	case corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
	default:
		fmt.Fprintf(os.Stderr, "invalid --seat-image-pull-policy %q\n", *pullPolicy)
		os.Exit(2)
	}

	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		log.Error(err, "scheme")
		os.Exit(1)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		log.Error(err, "scheme")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Cache:                   controller.CacheOptions(),
		Metrics:                 metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress:  *probeAddr,
		LeaderElection:          *leaderElect,
		LeaderElectionID:        "steadmesh-controller.steadmesh.io",
		LeaderElectionNamespace: *leaderNS,
	})
	if err != nil {
		log.Error(err, "create manager")
		os.Exit(1)
	}
	if err := controller.Setup(mgr, controller.Config{
		Platform: platform.New(*platformURL, *tokenFile),
		Backend: kube.Options{
			PlatformURL:           *platformURL,
			ControlPlaneNamespace: *cpNamespace,
			ImagePullPolicy:       corev1.PullPolicy(*pullPolicy),
			NetprobeImage:         *netprobeImage,
			NetprobeDeniedURL:     *netprobeDenied,
			DNSNamespace:          *dnsNamespace,
			EgressURL:             *egressURL,
		},
	}); err != nil {
		log.Error(err, "set up controllers")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "healthz")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "readyz")
		os.Exit(1)
	}
	log.Info("starting controller", "platformURL", *platformURL, "controlPlaneNamespace", *cpNamespace, "leaderElection", *leaderElect)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
}
