/*
Copyright The FleetPermit Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command fleetpermit-controller runs on the fleet hub. It watches
// FleetAccessPolicy and ToolAccessLease objects, resolves Open Cluster
// Management placements and delivers XAccessPolicy grants with ManifestWork.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	workv1 "open-cluster-management.io/api/work/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/controller"
	"github.com/fleetpermit/fleetpermit/internal/enforcement/agenticnetworking"
	"github.com/fleetpermit/fleetpermit/internal/placement/ocm"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		metricsAddr    string
		probeAddr      string
		leaderElect    bool
		leaderElectNS  string
		executorSA     string
		watchNamespace string
		showVersion    bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for the Prometheus metrics endpoint. Use 0 to disable.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for the liveness and readiness probes.")
	flag.BoolVar(&leaderElect, "leader-elect", true, "Enable leader election so only one replica reconciles at a time.")
	flag.StringVar(&leaderElectNS, "leader-election-namespace", "", "Namespace for the leader election Lease. Defaults to the pod namespace.")
	flag.StringVar(&executorSA, "work-executor", "", "Optional namespace/name of a ServiceAccount on managed clusters that the OCM work agent applies grants as.")
	flag.StringVar(&watchNamespace, "watch-namespace", "", "Restrict FleetPermit objects to one namespace. Empty watches all namespaces.")
	flag.BoolVar(&showVersion, "version", false, "Print the version and exit.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		return
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	shutdownTracing, err := setupTracing(context.Background())
	if err != nil {
		log.Error(err, "tracing disabled")
	}
	defer shutdownTracing()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(fpv1.AddToScheme(scheme))
	utilruntime.Must(clusterv1.Install(scheme))
	utilruntime.Must(clusterv1beta1.Install(scheme))
	utilruntime.Must(workv1.Install(scheme))

	var executor *workv1.ManifestWorkExecutor
	if executorSA != "" {
		ns, name, ok := strings.Cut(executorSA, "/")
		if !ok || ns == "" || name == "" {
			log.Error(nil, "--work-executor must be namespace/name", "value", executorSA)
			os.Exit(2)
		}
		executor = &workv1.ManifestWorkExecutor{Subject: workv1.ManifestWorkExecutorSubject{
			Type:           workv1.ExecutorSubjectTypeServiceAccount,
			ServiceAccount: &workv1.ManifestWorkSubjectServiceAccount{Namespace: ns, Name: name},
		}}
	}

	// Only FleetPermit's own ManifestWorks are cached, never the rest of the hub's.
	byObject := map[client.Object]cache.ByObject{
		&workv1.ManifestWork{}: {Label: labels.SelectorFromSet(labels.Set{ocm.LabelManagedBy: ocm.ManagedByValue})},
	}
	cacheOpts := cache.Options{ByObject: byObject}
	if watchNamespace != "" {
		ns := map[string]cache.Config{watchNamespace: {}}
		byObject[&fpv1.FleetAccessPolicy{}] = cache.ByObject{Namespaces: ns}
		byObject[&fpv1.ToolAccessLease{}] = cache.ByObject{Namespaces: ns}
		byObject[&clusterv1beta1.Placement{}] = cache.ByObject{Namespaces: ns}
		byObject[&clusterv1beta1.PlacementDecision{}] = cache.ByObject{Namespaces: ns}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "fleetpermit-controller.fleetpermit.github.io",
		LeaderElectionNamespace: leaderElectNS,
		// Step down on shutdown so a replacement pod (rolling update or
		// restart) takes over at once instead of waiting for the lease to
		// expire. Safe because the process exits as soon as the manager stops.
		LeaderElectionReleaseOnCancel: true,
		Cache:                         cacheOpts,
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	if err := controller.IndexLeases(ctx, mgr); err != nil {
		log.Error(err, "unable to index leases")
		os.Exit(1)
	}
	r := &controller.PolicyReconciler{
		Client:    mgr.GetClient(),
		Placement: &ocm.Provider{Client: mgr.GetClient(), Executor: executor},
		Renderer:  agenticnetworking.Renderer{},
		Now:       time.Now,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up controller")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to add health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "unable to add ready check")
		os.Exit(1)
	}

	log.Info("starting fleetpermit-controller", "version", version)
	if err := mgr.Start(ctx); err != nil {
		log.Error(err, "manager exited with an error")
		os.Exit(1)
	}
}

// setupTracing exports spans over OTLP/HTTP when OTEL_EXPORTER_OTLP_ENDPOINT
// (or the traces-specific variable) is set. Otherwise tracing is a no-op.
func setupTracing(ctx context.Context) (func(), error) {
	noop := func() {}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return noop, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return noop, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName("fleetpermit-controller"), semconv.ServiceVersion(version)))
	if err != nil {
		return noop, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(sctx)
	}, nil
}
