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

package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/controller"
)

// TestUpgradeFromV010 installs the v0.1.0 CRDs (testdata/crd-v0.1.0, copied
// from the v0.1.0 tag), creates objects that v0.1.0 accepted but the current
// CRDs reject, upgrades the CRDs and runs the controller. The controller must
// still manage these grandfathered objects: add and remove its finalizer, so
// that they can be deleted, and write lease status. It runs its own API
// server so that the shared one keeps the current CRDs.
func TestUpgradeFromV010(t *testing.T) {
	ctx := context.Background()
	env := &envtest.Environment{
		CRDDirectoryPaths:     append([]string{filepath.Join("testdata", "crd-v0.1.0")}, ocmCRDDirs()...),
		ErrorIfCRDPathMissing: true,
	}
	upCfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = env.Stop() }()
	c, err := client.New(upCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "upgrade"}}); err != nil {
		t.Fatal(err)
	}

	// Standing policies with six subjects. v0.1.0 defaulted their lease
	// durations as "15m" and "1h"; a client that writes the object back
	// sends "15m0s" and "1h0m0s".
	no := false
	standing := func(name string, finalizers ...string) *fpv1.FleetAccessPolicy {
		return &fpv1.FleetAccessPolicy{
			ObjectMeta: metav1.ObjectMeta{Namespace: "upgrade", Name: name, Finalizers: finalizers},
			Spec: fpv1.FleetAccessPolicySpec{
				Subjects:    subjects(6),
				Placement:   fpv1.PlacementSpec{PlacementRef: fpv1.LocalObjectReference{Name: "production-clusters"}},
				Target:      fpv1.TargetSpec{Namespace: "mcp-tools", Ref: fpv1.TargetRef{Name: "fleet-tools"}},
				Permissions: []fpv1.Permission{{Tool: "get_cluster_health"}},
				Lease:       fpv1.LeaseSettings{Required: &no},
			},
		}
	}
	// One was already handled by a v0.1.0 controller, one was not.
	finalized := standing("finalized-by-v010", controller.Finalizer)
	fresh := standing("never-reconciled")
	short := &fpv1.ToolAccessLease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "upgrade", Name: "five-seconds"},
		Spec: fpv1.ToolAccessLeaseSpec{
			PolicyRef: fpv1.LocalObjectReference{Name: fresh.Name}, Subject: fpv1.Subject{SPIFFEID: fresh.Spec.Subjects[0].SPIFFEID},
			Permissions: []fpv1.Permission{{Tool: "get_cluster_health"}}, Duration: &metav1.Duration{Duration: 5 * time.Second},
		},
	}
	for _, o := range []client.Object{finalized, fresh, short} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatalf("the v0.1.0 CRDs rejected %s: %v", o.GetName(), err)
		}
	}

	if _, err := envtest.InstallCRDs(upCfg, envtest.CRDInstallOptions{
		Paths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfPathMissing: true,
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the current CRDs to be in effect", func() error {
		probe := standing("probe")
		if err := c.Create(ctx, probe); err == nil {
			_ = c.Delete(ctx, probe)
			return fmt.Errorf("a standing policy with six subjects is still accepted")
		}
		return nil
	})

	stop := runController(t, upCfg, time.Now, "")
	defer stop()
	eventually(t, 15*time.Second, "the finalizer on the policy v0.1.0 never reconciled", func() error {
		var got fpv1.FleetAccessPolicy
		if err := c.Get(ctx, clientKey(fresh), &got); err != nil {
			return err
		}
		if !controllerutil.ContainsFinalizer(&got, controller.Finalizer) {
			return fmt.Errorf("finalizers %v", got.Finalizers)
		}
		return nil
	})
	eventually(t, 15*time.Second, "the lease status to be written", func() error {
		var got fpv1.ToolAccessLease
		if err := c.Get(ctx, clientKey(short), &got); err != nil {
			return err
		}
		if got.Status.Phase == "" {
			return fmt.Errorf("no phase yet")
		}
		return nil
	})
	for _, p := range []*fpv1.FleetAccessPolicy{finalized, fresh} {
		if err := c.Delete(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, 15*time.Second, "both policies to be deleted", func() error {
		for _, p := range []*fpv1.FleetAccessPolicy{finalized, fresh} {
			if err := c.Get(ctx, clientKey(p), &fpv1.FleetAccessPolicy{}); !apierrors.IsNotFound(err) {
				return fmt.Errorf("%s is still present (%v)", p.Name, err)
			}
		}
		return nil
	})
}
