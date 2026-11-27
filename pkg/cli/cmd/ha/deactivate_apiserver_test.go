// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ha

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	mclv1alpha1 "github.com/gke-labs/multicluster-leader-election/api/v1alpha1"
	mclstorage "github.com/gke-labs/multicluster-leader-election/pkg/storage"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestDeactivateOnAPIServers runs `ha deactivate` against two real envtest API servers in
// namespaced mode with the fake lease store: first a full deactivation run, then a status check
// confirming HA Status: DEACTIVATED, and finally a second run that makes no changes.
func TestDeactivateOnAPIServers(t *testing.T) {
	scheme, err := newHAScheme()
	if err != nil {
		t.Fatalf("newHAScheme() failed: %v", err)
	}
	fleet := newEmptyHAFleet(t)
	for _, name := range []string{"cluster-a", "cluster-b"} {
		server := startAPIServer(t)
		server.installHACRDs(t)
		backend, err := client.NewWithWatch(server.restConfig, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatalf("creating the client of %s: %v", name, err)
		}
		c := fleet.addCluster(name, backend)
		cc := newConfigConnectorWithMCL(fleetLeaseName, fleetLeaseNamespace, name)
		cc.Spec.Mode = "namespaced"
		c.create(cc)
		c.create(&mclv1alpha1.MultiClusterLease{ObjectMeta: metav1.ObjectMeta{Namespace: fleetLeaseNamespace, Name: fleetLeaseName}})
		c.create(newMCLDeployment(fleet.leaseBucket))
		// Namespaced mode, including a manager in its own namespace.
		c.create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b-system"}})
		c.create(newManagerStatefulSet("cnrm-system", "cnrm-controller-manager-team-a", "team-a"))
		c.create(newManagerStatefulSet("team-b-system", "cnrm-controller-manager-team-b", "team-b"))
	}
	fleet.settle()
	a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")
	fleet.requireLeader(a)

	// First run: full deactivation.
	out, err := fleet.deactivate("cluster-a", func(opts *deactivateOptions) {
		opts.timeout = time.Minute
		opts.podDeleteDelay = 500 * time.Millisecond
	})
	if err != nil {
		t.Fatalf("first deactivate run failed: %v\noutput:\n%s", err, out)
	}
	fleet.requireDeactivatedState(a, b)

	// Verify ha status reports DEACTIVATED on the real API servers.
	fleet.settle()
	report := executeStatus(context.Background(), fleet.handles(), fleet.renewTimeVal())
	var statusOut bytes.Buffer
	report.render(&statusOut)
	if report.state != haStateDeactivated {
		t.Fatalf("ha status state = %q, want %q\noutput:\n%s", report.state, haStateDeactivated, statusOut.String())
	}
	if !strings.Contains(statusOut.String(), "HA Status: DEACTIVATED") {
		t.Fatalf("ha status output did not contain HA Status: DEACTIVATED:\n%s", statusOut.String())
	}

	// Capture state before the second run to confirm it changes nothing.
	leaseBefore, err := fleet.leaseStore.ReadLease(context.Background(), fleetLeaseKey)
	if err != nil {
		t.Fatalf("reading lease before second run: %v", err)
	}
	ccABeforeRV := a.configConnector().ResourceVersion
	ccBBeforeRV := b.configConnector().ResourceVersion
	patchesBefore := len(fleet.patches)

	wrapped := &hookLeaseStorage{Storage: fleet.leaseStore}
	var secondRunUpdates, secondRunDeletes int
	for _, c := range fleet.clusters {
		c.rejectUpdate = func(obj client.Object) error {
			secondRunUpdates++
			return nil
		}
		c.rejectDelete = func(obj client.Object) error {
			secondRunDeletes++
			return nil
		}
	}

	out2, err := fleet.deactivate("cluster-a", func(opts *deactivateOptions) {
		opts.timeout = time.Minute
		opts.newLeaseStore = func(ctx context.Context, bucket string) (mclstorage.Storage, func() error, error) {
			return wrapped, nil, nil
		}
	})
	if err != nil {
		t.Fatalf("second deactivate run failed: %v\noutput:\n%s", err, out2)
	}

	if wrapped.writes != 0 {
		t.Errorf("second run wrote to lease store %d times, want 0", wrapped.writes)
	}
	leaseAfter, err := fleet.leaseStore.ReadLease(context.Background(), fleetLeaseKey)
	if err != nil {
		t.Fatalf("reading lease after second run: %v", err)
	}
	if leaseAfter.ResourceVersion != leaseBefore.ResourceVersion {
		t.Errorf("second run changed lease ResourceVersion from %s to %s", leaseBefore.ResourceVersion, leaseAfter.ResourceVersion)
	}
	if len(fleet.patches) != patchesBefore {
		t.Errorf("second run made %d patches, want 0", len(fleet.patches)-patchesBefore)
	}
	if secondRunUpdates != 0 {
		t.Errorf("second run made %d Updates, want 0", secondRunUpdates)
	}
	if secondRunDeletes != 0 {
		t.Errorf("second run made %d Deletes, want 0", secondRunDeletes)
	}
	if a.configConnector().ResourceVersion != ccABeforeRV {
		t.Errorf("second run changed cluster-a ConfigConnector RV from %s to %s", ccABeforeRV, a.configConnector().ResourceVersion)
	}
	if b.configConnector().ResourceVersion != ccBBeforeRV {
		t.Errorf("second run changed cluster-b ConfigConnector RV from %s to %s", ccBBeforeRV, b.configConnector().ResourceVersion)
	}
	fleet.requireDeactivatedState(a, b)
}
