//go:build integration

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

// Package integration runs FleetPermit against a real kube-apiserver and etcd
// (envtest) with the FleetPermit CRDs, the Open Cluster Management CRDs and
// the upstream kube-agentic-networking XAccessPolicy CRD installed.
package integration

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	workv1 "open-cluster-management.io/api/work/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/controller"
	"github.com/fleetpermit/fleetpermit/internal/enforcement/agenticnetworking"
	"github.com/fleetpermit/fleetpermit/internal/placement/ocm"
)

var (
	cfg    *rest.Config
	k8s    client.Client
	scheme = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(fpv1.AddToScheme(scheme))
	utilruntime.Must(clusterv1.Install(scheme))
	utilruntime.Must(clusterv1beta1.Install(scheme))
	utilruntime.Must(workv1.Install(scheme))
	logOpts := []zap.Opts{zap.UseDevMode(false)}
	if os.Getenv("FP_TEST_DEBUG") == "" {
		logOpts = append(logOpts, zap.WriteTo(io.Discard))
	}
	ctrl.SetLogger(zap.New(logOpts...))

	root, err := filepath.Abs("../..")
	if err != nil {
		panic(err)
	}
	ocmDir, err := moduleDir("open-cluster-management.io/api")
	if err != nil {
		fmt.Fprintln(os.Stderr, "locating the OCM API module:", err)
		os.Exit(1)
	}
	upstream := filepath.Join(root, "test", "fixtures", "upstream")
	if dir := os.Getenv("FP_UPSTREAM_CRD_DIR"); dir != "" {
		upstream = dir // used by hack/upstream-canary.sh
	}
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join(root, "config", "crd"),
			upstream,
			filepath.Join(ocmDir, "cluster", "v1"),
			filepath.Join(ocmDir, "cluster", "v1beta1"),
			filepath.Join(ocmDir, "work", "v1"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err = env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting envtest (run via `make test-integration`):", err)
		os.Exit(1)
	}
	k8s, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

// ctrlConfig allows several managers (restart tests) in one process.
func ctrlConfig() config.Controller {
	skip := true
	return config.Controller{SkipNameValidation: &skip}
}

func moduleDir(mod string) (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", mod).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// fakeClock is a settable clock shared by the reconciler and the test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// startController runs the FleetPermit reconciler in-process and returns a
// stop function.
func startController(t testing.TB, clock func() time.Time) func() {
	t.Helper()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:     scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: ctrlConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := controller.IndexLeases(ctx, mgr); err != nil {
		t.Fatal(err)
	}
	r := &controller.PolicyReconciler{
		Client:    mgr.GetClient(),
		Placement: &ocm.Provider{Client: mgr.GetClient()},
		Renderer:  agenticnetworking.Renderer{},
		Now:       clock,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("manager: %v", err)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func createNamespace(t testing.TB, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8s.Create(context.Background(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

// pollInterval is how often eventually re-checks; it bounds timing resolution.
var pollInterval = 100 * time.Millisecond

// eventually polls cond until it returns nil or the timeout expires.
func eventually(t testing.TB, timeout time.Duration, what string, cond func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = cond(); last == nil {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("timed out waiting for %s: %v", what, last)
}
