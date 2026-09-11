package main

import (
	"flag"
	"os"

	optiscalev1alpha1 "github.com/RajRaghupatruni/Silver-Leaf/api/v1alpha1"
	"github.com/RajRaghupatruni/Silver-Leaf/internal/controller"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	var metricsAddr string
	var probeAddr string
	var watchNamespace string
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&watchNamespace, "watch-namespace", os.Getenv("WATCH_NAMESPACE"), "Namespace to watch; empty watches all namespaces.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election.")
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true})))

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = autoscalingv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = optiscalev1alpha1.AddToScheme(scheme)
	cacheOptions := cache.Options{}
	if watchNamespace != "" {
		cacheOptions.DefaultNamespaces = map[string]cache.Config{watchNamespace: {}}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), manager.Options{
		Scheme:                 scheme,
		Cache:                  cacheOptions,
		Metrics:                server.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "optiscale.silver-leaf.io",
	})
	if err != nil {
		os.Exit(1)
	}
	if err := (&controller.OptiScalerReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: mgr.GetEventRecorderFor("optiscaler-controller")}).SetupWithManager(mgr); err != nil {
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		os.Exit(1)
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		os.Exit(1)
	}
}
