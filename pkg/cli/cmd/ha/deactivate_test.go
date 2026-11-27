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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	operatorv1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/operator/pkg/apis/core/v1beta1"
	syncerv1alpha1 "github.com/gke-labs/kube-etl/syncer/api/v1alpha1"
	mclstorage "github.com/gke-labs/multicluster-leader-election/pkg/storage"
	"google.golang.org/api/googleapi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDeactivateValidateNormalizesFields(t *testing.T) {
	opts := newDeactivateOptions()
	opts.contexts = []string{" cluster-a "}
	opts.standaloneCluster = " cluster-a "
	opts.bucket = " gs://my-lease-bucket/ "

	if err := opts.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(opts.contexts, []string{"cluster-a"}) {
		t.Fatalf("got contexts %v, want [cluster-a]", opts.contexts)
	}
	if opts.standaloneCluster != "cluster-a" {
		t.Fatalf("got standaloneCluster %q, want %q", opts.standaloneCluster, "cluster-a")
	}
	if opts.bucket != "my-lease-bucket" {
		t.Fatalf("got bucket %q, want %q", opts.bucket, "my-lease-bucket")
	}
}

func TestDeactivateValidateErrors(t *testing.T) {
	tests := []struct {
		name              string
		contexts          []string
		standaloneCluster string
		timeout           time.Duration
		bucket            string
		wantErrSubstrings []string
	}{
		{
			name:              "no contexts",
			contexts:          nil,
			standaloneCluster: "cluster-a",
			timeout:           time.Minute,
			wantErrSubstrings: []string{"at least 1 cluster context", "must be one of the contexts"},
		},
		{
			name:              "missing standalone-cluster",
			contexts:          []string{"cluster-a", "cluster-b"},
			timeout:           time.Minute,
			wantErrSubstrings: []string{"--standalone-cluster is required"},
		},
		{
			name:              "standalone-cluster not in contexts",
			contexts:          []string{"cluster-a", "cluster-b"},
			standaloneCluster: "cluster-c",
			timeout:           time.Minute,
			wantErrSubstrings: []string{"--standalone-cluster \"cluster-c\" must be one of the contexts"},
		},
		{
			name:              "duplicate contexts",
			contexts:          []string{"cluster-a", "cluster-a"},
			standaloneCluster: "cluster-a",
			timeout:           time.Minute,
			wantErrSubstrings: []string{`duplicate context "cluster-a"`},
		},
		{
			name:              "timeout below 1m",
			contexts:          []string{"cluster-a"},
			standaloneCluster: "cluster-a",
			timeout:           30 * time.Second,
			wantErrSubstrings: []string{"--timeout must be at least 1m0s, got 30s"},
		},
		{
			name:              "invalid bucket name",
			contexts:          []string{"cluster-a"},
			standaloneCluster: "cluster-a",
			timeout:           time.Minute,
			bucket:            "INVALID_BUCKET!",
			wantErrSubstrings: []string{"invalid GCS bucket name"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := newDeactivateOptions()
			opts.contexts = slices.Clone(tc.contexts)
			opts.standaloneCluster = tc.standaloneCluster
			opts.timeout = tc.timeout
			opts.bucket = tc.bucket

			err := opts.validate()
			requireErrorContains(t, err, tc.wantErrSubstrings...)
		})
	}
}

func TestDeactivateFlagDefaultsAndHelpText(t *testing.T) {
	cmd := NewDeactivateCmd()
	flags := cmd.Flags()
	if got, want := flags.Lookup(failoverTimeoutFlag).DefValue, "5m0s"; got != want {
		t.Fatalf("--timeout default = %q, want %q", got, want)
	}
	if got, want := flags.Lookup(dryRunFlag).DefValue, "false"; got != want {
		t.Fatalf("--dry-run default = %q, want %q", got, want)
	}
	for _, flagName := range []string{contextsFlag, standaloneClusterFlag, failoverTimeoutFlag, dryRunFlag, bucketFlag} {
		if flags.Lookup(flagName) == nil {
			t.Fatalf("expected flag --%s to be registered", flagName)
		}
	}

	// Verify the help text covers all four required points.
	for _, want := range []string{
		"without leader election",
		"must already be the leader",
		"keep their copies of the resources",
		"Running this command again is safe",
	} {
		if !strings.Contains(cmd.Long, want) {
			t.Errorf("help text missing %q:\n%s", want, cmd.Long)
		}
	}
}

// fleetMode configures an haFleet either in cluster mode or in namespaced mode (including a
// manager in its own namespace).
type fleetMode struct {
	name       string
	namespaced bool
}

var bothFleetModes = []fleetMode{
	{name: "cluster mode", namespaced: false},
	{name: "namespaced mode", namespaced: true},
}

func newDeactivateFleet(t *testing.T, mode fleetMode, names ...string) *haFleet {
	t.Helper()
	fleet := newHAFleet(t, names...)
	if mode.namespaced {
		for _, c := range fleet.clusters {
			cc := c.configConnector()
			cc.Spec.Mode = "namespaced"
			if err := c.backend.Update(context.Background(), cc); err != nil {
				t.Fatalf("cluster %q: switching ConfigConnector to namespaced mode: %v", c.name, err)
			}
			c.useNamespacedMode(map[string]string{
				"team-a": fleetLeaseNamespace,
				"team-b": "team-b-system",
			})
		}
		fleet.settle()
	}
	return fleet
}

func (c *fleetCluster) configConnector() *operatorv1beta1.ConfigConnector {
	c.fleet.t.Helper()
	cc := &operatorv1beta1.ConfigConnector{}
	if err := c.backend.Get(context.Background(), types.NamespacedName{Name: configConnectorResourceName}, cc); err != nil {
		c.fleet.t.Fatalf("cluster %q: getting ConfigConnector: %v", c.name, err)
	}
	return cc
}

func (c *fleetCluster) mclDeployment() *appsv1.Deployment {
	c.fleet.t.Helper()
	dep := &appsv1.Deployment{}
	if err := c.backend.Get(context.Background(), types.NamespacedName{Namespace: manifestSystemNamespace, Name: mclDeploymentName}, dep); err != nil {
		c.fleet.t.Fatalf("cluster %q: getting MCL Deployment: %v", c.name, err)
	}
	return dep
}

func (f *haFleet) deactivate(standalone string, options ...func(*deactivateOptions)) (string, error) {
	f.t.Helper()
	return f.deactivateOn(context.Background(), f.handles(), standalone, options...)
}

func (f *haFleet) deactivateOn(ctx context.Context, handles []*clusterHandle, standalone string, options ...func(*deactivateOptions)) (string, error) {
	f.t.Helper()
	var out bytes.Buffer
	opts := newDeactivateOptions()
	opts.standaloneCluster = standalone
	opts.stdout = &out
	opts.timeout = 10 * time.Second
	opts.pollInterval = time.Millisecond
	opts.leaseCloseTimeout = 5 * time.Second
	opts.leaseRetryInterval = time.Millisecond
	opts.podDeleteDelay = 20 * time.Millisecond
	opts.newLeaseStore = func(ctx context.Context, bucket string) (mclstorage.Storage, func() error, error) {
		return f.leaseStore, nil, nil
	}
	for _, option := range options {
		option(&opts)
	}
	err := executeDeactivate(ctx, opts, handles)
	return out.String(), err
}

// requireDeactivatedState verifies the full expected end-state after ha deactivate succeeds on
// standalone and parks all other clusters in contexts.
func (f *haFleet) requireDeactivatedState(standalone *fleetCluster, parked ...*fleetCluster) {
	t := f.t
	t.Helper()

	// 1. Lease is closed for standalone with renewTime 9999-12-31T00:00:00Z.
	leaseObj, err := f.leaseStore.ReadLease(context.Background(), fleetLeaseKey)
	if err != nil {
		t.Fatalf("reading lease from store: %v", err)
	}
	if got, want := leaseObj.Data.HolderIdentity, closedLeaseHolder(standalone.identity()); got != want {
		t.Errorf("lease holderIdentity = %q, want %q", got, want)
	}
	if !leaseObj.Data.RenewTime.Equal(deactivatedLeaseRenewTime) {
		t.Errorf("lease renewTime = %s, want %s", leaseObj.Data.RenewTime.UTC().Format(time.RFC3339), deactivatedLeaseRenewTime.Format(time.RFC3339))
	}

	// 2. Standalone cluster's ConfigConnector has deactivatedLeaseAnnotation and no multiClusterLease.
	cc := standalone.configConnector()
	if multiClusterLeaseSpec(cc) != nil {
		t.Errorf("standalone cluster %q still has spec.experiments.multiClusterLease", standalone.name)
	}
	savedSpec, ok, err := deactivatedLeaseSpec(standalone.name, cc)
	if err != nil || !ok {
		t.Fatalf("standalone cluster %q missing valid %s annotation: present=%v err=%v", standalone.name, deactivatedLeaseAnnotation, ok, err)
	}
	if savedSpec.LeaseName != fleetLeaseName || savedSpec.Namespace != fleetLeaseNamespace {
		t.Errorf("saved lease spec = %+v, want %s/%s", savedSpec, fleetLeaseNamespace, fleetLeaseName)
	}
	if id, _ := savedSpec.CandidateIdentity(); id != standalone.identity() {
		t.Errorf("saved lease identity = %q, want %q", id, standalone.identity())
	}

	// Standalone's own KRMSyncers are suspended.
	for _, s := range standalone.syncers() {
		if !s.Spec.Suspend {
			t.Errorf("standalone cluster %q: KRMSyncer %s is not suspended", standalone.name, s.Name)
		}
	}

	// 3. Parked clusters have managers scaled to 0 with annotation, KRMSyncers suspended, and MCL
	// Deployment scaled to 0 with annotation, while keeping spec.experiments.multiClusterLease.
	for _, c := range parked {
		pcc := c.configConnector()
		if multiClusterLeaseSpec(pcc) == nil {
			t.Errorf("parked cluster %q lost spec.experiments.multiClusterLease", c.name)
		}
		for _, sts := range c.managers() {
			if *sts.Spec.Replicas != 0 {
				t.Errorf("parked cluster %q: StatefulSet %s/%s replicas = %d, want 0", c.name, sts.Namespace, sts.Name, *sts.Spec.Replicas)
			}
			if c.hasPod(&sts) {
				t.Errorf("parked cluster %q: StatefulSet %s/%s still has a running pod", c.name, sts.Namespace, sts.Name)
			}
			if got := sts.Annotations[deactivatedReplicasAnnotation]; got != "1" {
				t.Errorf("parked cluster %q: StatefulSet %s/%s annotation %s = %q, want \"1\"", c.name, sts.Namespace, sts.Name, deactivatedReplicasAnnotation, got)
			}
		}
		for _, s := range c.syncers() {
			if !s.Spec.Suspend {
				t.Errorf("parked cluster %q: KRMSyncer %s is not suspended", c.name, s.Name)
			}
		}
		dep := c.mclDeployment()
		if deploymentReplicas(dep) != 0 {
			t.Errorf("parked cluster %q: MCL Deployment replicas = %d, want 0", c.name, deploymentReplicas(dep))
		}
		if got := dep.Annotations[deactivatedReplicasAnnotation]; got != "1" {
			t.Errorf("parked cluster %q: MCL Deployment annotation %s = %q, want \"1\"", c.name, deactivatedReplicasAnnotation, got)
		}
	}

	// 4. Only standalone's managers reconcile, and in standalone mode.
	if !standalone.isStandaloneReconciling() {
		t.Errorf("standalone cluster %q is not reconciling in standalone mode", standalone.name)
	}
	if got := f.reconcilingClusters(); !slices.Equal(got, []string{standalone.name}) {
		t.Errorf("reconciling clusters = %v, want [%s]", got, standalone.name)
	}
}

// Test 1 & 2: Full run in cluster mode and namespaced mode, verifying ha status prints DEACTIVATED
// and never-two invariant holds throughout.
func TestDeactivateWorksAndStatusIsDeactivated(t *testing.T) {
	for _, mode := range bothFleetModes {
		t.Run(mode.name, func(t *testing.T) {
			fleet := newDeactivateFleet(t, mode, "cluster-a", "cluster-b")
			a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")
			fleet.requireLeader(a)

			out, err := fleet.deactivate("cluster-a")
			if err != nil {
				t.Fatalf("deactivate failed: %v\noutput:\n%s", err, out)
			}

			fleet.requireDeactivatedState(a, b)

			for _, want := range []string{
				"Leader: cluster-a",
				"Lease: gs://test-lease-bucket/leases/cnrm-system/kcc-multicluster-lease",
				"lease: closed, no cluster can take it",
				`cluster "cluster-a": removed spec.experiments.multiClusterLease from its ConfigConnector`,
				`cluster "cluster-a": reconciles without leader election`,
				"Multi-cluster HA is deactivated: cluster-a reconciles on its own.",
				"config-connector ha activate --contexts=cluster-a,cluster-b",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}

			// Step a few times so standalone's MCL controller sees the closed lease, then check ha status.
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
		})
	}
}

// Test 3: A member not in --contexts never leads during the run or for many lease expirations after.
func TestDeactivateMemberNotInContextsNeverLeads(t *testing.T) {
	for _, mode := range bothFleetModes {
		t.Run(mode.name, func(t *testing.T) {
			fleet := newDeactivateFleet(t, mode, "cluster-a", "cluster-b", "cluster-c")
			a, b, c := fleet.cluster("cluster-a"), fleet.cluster("cluster-b"), fleet.cluster("cluster-c")
			fleet.requireLeader(a)

			// Only pass cluster-a and cluster-b; cluster-c is omitted from --contexts.
			out, err := fleet.deactivateOn(context.Background(), []*clusterHandle{a.handle, b.handle}, "cluster-a")
			if err != nil {
				t.Fatalf("deactivate failed: %v\noutput:\n%s", err, out)
			}

			fleet.requireDeactivatedState(a, b)

			// Advance far beyond many ordinary lease expirations while cluster-c stays running.
			for i := 0; i < fleet.takeOverSteps*20; i++ {
				fleet.step()
				if c.isReconciling() {
					t.Fatalf("omitted member cluster-c started reconciling at step %d", fleet.steps)
				}
			}
			if !c.running() {
				t.Fatalf("expected omitted member cluster-c's managers to still be running")
			}
			if slices.Contains(fleet.holders, "cluster-c") {
				t.Fatalf("omitted member cluster-c held the lease at some point: holders=%v", fleet.holders)
			}
			if got := fleet.reconcilingClusters(); !slices.Equal(got, []string{"cluster-a"}) {
				t.Fatalf("reconciling clusters = %v, want [cluster-a]", got)
			}
		})
	}
}

// Test 4: Killed after each step, and after each cluster of step 3; verify intermediate state and
// that a second run completes.
func TestDeactivateKilledAfterEachStep(t *testing.T) {
	killErr := errors.New("simulated kill")

	for _, mode := range bothFleetModes {
		t.Run(mode.name, func(t *testing.T) {
			killPoints := []struct {
				step        string
				checkMiddle func(t *testing.T, fleet *haFleet)
			}{
				{
					step: "step1",
					checkMiddle: func(t *testing.T, fleet *haFleet) {
						t.Helper()
						a, b, c := fleet.cluster("cluster-a"), fleet.cluster("cluster-b"), fleet.cluster("cluster-c")
						// Lease is closed, X still has multiClusterLease.
						obj, err := fleet.leaseStore.ReadLease(context.Background(), fleetLeaseKey)
						if err != nil {
							t.Fatalf("reading lease: %v", err)
						}
						if obj.Data.HolderIdentity != closedLeaseHolder(a.identity()) {
							t.Fatalf("lease holder = %q, want closed for cluster-a", obj.Data.HolderIdentity)
						}
						if multiClusterLeaseSpec(a.configConnector()) == nil {
							t.Fatalf("expected cluster-a to still have spec.experiments.multiClusterLease after step 1")
						}
						// After lease loss settles, X's managers restart as candidates and NO cluster reconciles.
						for i := 0; i < fleet.leaseLossSteps+fleet.takeOverSteps+2; i++ {
							fleet.step()
						}
						if got := fleet.reconcilingClusters(); len(got) != 0 {
							t.Fatalf("after dying between step 1 and 2, reconciling clusters = %v, want none", got)
						}
						if !b.running() || !c.running() {
							t.Fatalf("expected cluster-b and cluster-c to not be parked yet")
						}
					},
				},
				{
					step: "step2",
					checkMiddle: func(t *testing.T, fleet *haFleet) {
						t.Helper()
						a, b, c := fleet.cluster("cluster-a"), fleet.cluster("cluster-b"), fleet.cluster("cluster-c")
						if multiClusterLeaseSpec(a.configConnector()) != nil {
							t.Fatalf("expected cluster-a to have removed multiClusterLease after step 2")
						}
						// X becomes standalone on its own; others are not parked and cannot lead.
						for i := 0; i < fleet.leaseLossSteps+fleet.takeOverSteps+2; i++ {
							fleet.step()
						}
						if !a.isStandaloneReconciling() {
							t.Fatalf("expected cluster-a to become standalone on its own after step 2")
						}
						if !b.running() || !c.running() {
							t.Fatalf("expected cluster-b and cluster-c to still be unparked after step 2")
						}
						if got := fleet.reconcilingClusters(); !slices.Equal(got, []string{"cluster-a"}) {
							t.Fatalf("reconciling clusters = %v, want [cluster-a]", got)
						}
					},
				},
				{
					step: "step3:cluster-b",
					checkMiddle: func(t *testing.T, fleet *haFleet) {
						t.Helper()
						b, c := fleet.cluster("cluster-b"), fleet.cluster("cluster-c")
						if b.running() {
							t.Fatalf("expected cluster-b to be parked")
						}
						if !c.running() {
							t.Fatalf("expected cluster-c to still be unparked")
						}
					},
				},
				{
					step: "step3:cluster-c",
					checkMiddle: func(t *testing.T, fleet *haFleet) {
						t.Helper()
						b, c := fleet.cluster("cluster-b"), fleet.cluster("cluster-c")
						if b.running() || c.running() {
							t.Fatalf("expected both cluster-b and cluster-c to be parked")
						}
					},
				},
			}

			for _, kp := range killPoints {
				t.Run("killed after "+kp.step, func(t *testing.T) {
					fleet := newDeactivateFleet(t, mode, "cluster-a", "cluster-b", "cluster-c")
					a, b, c := fleet.cluster("cluster-a"), fleet.cluster("cluster-b"), fleet.cluster("cluster-c")

					_, err := fleet.deactivate("cluster-a", func(opts *deactivateOptions) {
						opts.onAfterStep = func(step string) error {
							if step == kp.step {
								return killErr
							}
							return nil
						}
					})
					if !errors.Is(err, killErr) {
						t.Fatalf("got error %v, want %v", err, killErr)
					}

					kp.checkMiddle(t, fleet)

					// Second run completes cleanly.
					out, err := fleet.deactivate("cluster-a")
					if err != nil {
						t.Fatalf("second run failed: %v\noutput:\n%s", err, out)
					}
					fleet.requireDeactivatedState(a, b, c)
				})
			}
		})
	}
}

// hookLeaseStorage wraps a Storage to inject concurrent renewals, leader changes, or errors.
type hookLeaseStorage struct {
	mclstorage.Storage
	onRead   func(ctx context.Context, key string) (*mclstorage.LeaseObject, error, bool)
	onUpdate func(ctx context.Context, key string, obj *mclstorage.LeaseObject) (*mclstorage.LeaseObject, error, bool)
	writes   int
}

func (s *hookLeaseStorage) ReadLease(ctx context.Context, key string) (*mclstorage.LeaseObject, error) {
	if s.onRead != nil {
		if obj, err, handled := s.onRead(ctx, key); handled {
			return obj, err
		}
	}
	return s.Storage.ReadLease(ctx, key)
}

func (s *hookLeaseStorage) UpdateLease(ctx context.Context, key string, obj *mclstorage.LeaseObject) (*mclstorage.LeaseObject, error) {
	s.writes++
	if s.onUpdate != nil {
		if updated, err, handled := s.onUpdate(ctx, key, obj); handled {
			return updated, err
		}
	}
	return s.Storage.UpdateLease(ctx, key, obj)
}

func (s *hookLeaseStorage) CreateLease(ctx context.Context, key string, data mclstorage.LeaseData) (*mclstorage.LeaseObject, error) {
	s.writes++
	return s.Storage.CreateLease(ctx, key, data)
}

func (s *hookLeaseStorage) DeleteLease(ctx context.Context, key string) error {
	s.writes++
	return s.Storage.DeleteLease(ctx, key)
}

// Test 5: A renewal lands between the read and the write of step 1: retried and succeeds.
func TestDeactivateStep1RetriesOnRenewalConflict(t *testing.T) {
	for _, mode := range bothFleetModes {
		t.Run(mode.name, func(t *testing.T) {
			fleet := newDeactivateFleet(t, mode, "cluster-a", "cluster-b")
			a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")

			conflicts := 0
			wrapped := &hookLeaseStorage{
				Storage: fleet.leaseStore,
				onUpdate: func(ctx context.Context, key string, obj *mclstorage.LeaseObject) (*mclstorage.LeaseObject, error, bool) {
					if conflicts < 2 {
						conflicts++
						// Simulate leader renewing the lease right before UpdateLease lands.
						cur, err := fleet.leaseStore.ReadLease(ctx, key)
						if err != nil {
							return nil, err, true
						}
						cur.Data.RenewTime = cur.Data.RenewTime.Add(2 * time.Second)
						if _, err := fleet.leaseStore.UpdateLease(ctx, key, cur); err != nil {
							return nil, err, true
						}
						// Now pass the stale obj so FakeStorage returns ErrConflict.
						updated, err := fleet.leaseStore.UpdateLease(ctx, key, obj)
						return updated, err, true
					}
					return nil, nil, false
				},
			}

			out, err := fleet.deactivate("cluster-a", func(opts *deactivateOptions) {
				opts.newLeaseStore = func(ctx context.Context, bucket string) (mclstorage.Storage, func() error, error) {
					return wrapped, nil, nil
				}
			})
			if err != nil {
				t.Fatalf("deactivate failed after renewal conflicts: %v\noutput:\n%s", err, out)
			}
			if conflicts != 2 {
				t.Fatalf("expected 2 simulated conflicts, got %d", conflicts)
			}
			fleet.requireDeactivatedState(a, b)
		})
	}
}

// Test 6: Leadership moves between step 0 and step 1: command stops, nothing changed.
func TestDeactivateLeadershipMovesBetweenStep0AndStep1(t *testing.T) {
	for _, mode := range bothFleetModes {
		t.Run(mode.name, func(t *testing.T) {
			fleet := newDeactivateFleet(t, mode, "cluster-a", "cluster-b")
			a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")

			readCount := 0
			wrapped := &hookLeaseStorage{
				Storage: fleet.leaseStore,
				onRead: func(ctx context.Context, key string) (*mclstorage.LeaseObject, error, bool) {
					readCount++
					// Read #1 is Step 0; Read #2 is Step 1. Move leadership to cluster-b right before Step 1 reads.
					if readCount == 2 {
						fleet.giveLease(b)
					}
					return nil, nil, false
				},
			}

			_, err := fleet.deactivate("cluster-a", func(opts *deactivateOptions) {
				opts.newLeaseStore = func(ctx context.Context, bucket string) (mclstorage.Storage, func() error, error) {
					return wrapped, nil, nil
				}
			})
			requireErrorContains(t, err, "leadership moved", b.identity(), "nothing was changed")

			if wrapped.writes != 0 {
				t.Errorf("expected 0 writes to lease store, got %d", wrapped.writes)
			}
			if multiClusterLeaseSpec(a.configConnector()) == nil {
				t.Errorf("cluster-a ConfigConnector should not have been modified")
			}
			if len(fleet.patches) != 0 {
				t.Errorf("expected no patches on any cluster, got %v", fleet.patches)
			}
		})
	}
}

// Test 7: Step 2 fails: the lease object is put back as it was read in step 1, and the group works as before.
func TestDeactivateStep2FailsRestoresLease(t *testing.T) {
	for _, mode := range bothFleetModes {
		t.Run(mode.name, func(t *testing.T) {
			fleet := newDeactivateFleet(t, mode, "cluster-a", "cluster-b")
			a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")

			beforeLease, err := fleet.leaseStore.ReadLease(context.Background(), fleetLeaseKey)
			if err != nil {
				t.Fatalf("reading lease before: %v", err)
			}

			// Allow Step 0 dry-run, fail the real Step 2 Update.
			updateCalls := 0
			a.rejectUpdate = func(obj client.Object) error {
				if _, ok := obj.(*operatorv1beta1.ConfigConnector); ok {
					updateCalls++
					if updateCalls >= 2 { // 1st call is Step 0 DryRunAll
						return errors.New("apiserver internal error")
					}
				}
				return nil
			}

			_, err = fleet.deactivate("cluster-a")
			requireErrorContains(t, err, "apiserver internal error", "restored")

			// Lease is back to cluster-a (not closed) with leaseTransitions preserved.
			afterLease, err := fleet.leaseStore.ReadLease(context.Background(), fleetLeaseKey)
			if err != nil {
				t.Fatalf("reading lease after: %v", err)
			}
			if afterLease.Data.HolderIdentity != a.identity() {
				t.Errorf("lease holder = %q, want %q", afterLease.Data.HolderIdentity, a.identity())
			}
			if afterLease.Data.LeaseTransitions != beforeLease.Data.LeaseTransitions {
				t.Errorf("lease transitions = %d, want %d", afterLease.Data.LeaseTransitions, beforeLease.Data.LeaseTransitions)
			}
			if multiClusterLeaseSpec(a.configConnector()) == nil {
				t.Errorf("cluster-a should still have spec.experiments.multiClusterLease")
			}
			// Group continues working as before.
			fleet.settle()
			fleet.requireLeader(a)
			if !b.running() {
				t.Errorf("cluster-b should still be running")
			}
		})
	}
}

func TestDeactivateStep2FailsAndRollbackFails(t *testing.T) {
	fleet := newHAFleet(t, "cluster-a", "cluster-b")
	a := fleet.cluster("cluster-a")

	updateCalls := 0
	a.rejectUpdate = func(obj client.Object) error {
		if _, ok := obj.(*operatorv1beta1.ConfigConnector); ok {
			updateCalls++
			if updateCalls >= 2 {
				return errors.New("configconnector update failed")
			}
		}
		return nil
	}

	leaseUpdates := 0
	wrapped := &hookLeaseStorage{
		Storage: fleet.leaseStore,
		onUpdate: func(ctx context.Context, key string, obj *mclstorage.LeaseObject) (*mclstorage.LeaseObject, error, bool) {
			leaseUpdates++
			if leaseUpdates == 2 { // 1st is Step 1 close, 2nd is rollback
				return nil, errors.New("gcs unavailable"), true
			}
			return nil, nil, false
		},
	}

	_, err := fleet.deactivate("cluster-a", func(opts *deactivateOptions) {
		opts.newLeaseStore = func(ctx context.Context, bucket string) (mclstorage.Storage, func() error, error) {
			return wrapped, nil, nil
		}
	})
	requireErrorContains(t, err, `the lease is closed and cluster "cluster-a" still has leader election configured: no cluster reconciles. Run the command again.`)
}

func TestDeactivateStep2RetriesOnConflict(t *testing.T) {
	fleet := newHAFleet(t, "cluster-a", "cluster-b")
	a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")

	updateCalls := 0
	a.rejectUpdate = func(obj client.Object) error {
		if _, ok := obj.(*operatorv1beta1.ConfigConnector); ok {
			updateCalls++
			if updateCalls == 2 { // 1st is Step 0 DryRunAll, 2nd is first real attempt
				return apierrors.NewConflict(schema.GroupResource{Group: "core.cnrm.cloud.google.com", Resource: "configconnectors"}, configConnectorResourceName, errors.New("rv conflict"))
			}
		}
		return nil
	}

	out, err := fleet.deactivate("cluster-a")
	if err != nil {
		t.Fatalf("deactivate failed after transient conflict: %v\noutput:\n%s", err, out)
	}
	fleet.requireDeactivatedState(a, b)
}

// Test 8: Every row of "Running it again", including a group deactivated by hand without closing the lease.
func TestDeactivateRunningItAgainTable(t *testing.T) {
	t.Run("row 1: expired lease naming X and X has lease", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")
		// Set renewTime far in the past (expired relative to real wall clock) without comparing local clock.
		obj, _ := fleet.leaseStore.ReadLease(context.Background(), fleetLeaseKey)
		obj.Data.RenewTime = time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
		_, _ = fleet.leaseStore.UpdateLease(context.Background(), fleetLeaseKey, obj)

		out, err := fleet.deactivate("cluster-a")
		if err != nil {
			t.Fatalf("deactivate failed: %v\noutput:\n%s", err, out)
		}
		fleet.requireDeactivatedState(a, b)
	})

	t.Run("row 2: lease closed for X, X still has lease in ConfigConnector", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")
		obj, _ := fleet.leaseStore.ReadLease(context.Background(), fleetLeaseKey)
		obj.Data.HolderIdentity = closedLeaseHolder(a.identity())
		obj.Data.RenewTime = deactivatedLeaseRenewTime
		_, _ = fleet.leaseStore.UpdateLease(context.Background(), fleetLeaseKey, obj)

		out, err := fleet.deactivate("cluster-a")
		if err != nil {
			t.Fatalf("deactivate failed: %v\noutput:\n%s", err, out)
		}
		fleet.requireDeactivatedState(a, b)
	})

	t.Run("row 3a: deactivated by hand without closing the lease", func(t *testing.T) {
		for _, mode := range bothFleetModes {
			t.Run(mode.name, func(t *testing.T) {
				fleet := newDeactivateFleet(t, mode, "cluster-a", "cluster-b")
				a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")

				// Manually remove multiClusterLease and set deactivatedLeaseAnnotation on X, leaving lease open.
				cc := a.configConnector()
				if err := deactivateConfigConnector(cc); err != nil {
					t.Fatalf("deactivateConfigConnector: %v", err)
				}
				if err := a.backend.Update(context.Background(), cc); err != nil {
					t.Fatalf("updating ConfigConnector: %v", err)
				}

				out, err := fleet.deactivate("cluster-a")
				if err != nil {
					t.Fatalf("deactivate failed: %v\noutput:\n%s", err, out)
				}
				fleet.requireDeactivatedState(a, b)
			})
		}
	})

	t.Run("row 3b: X standalone already, lease closed without identity -> rewritten with X identity", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")

		// Close lease with bare marker (no identity) and make X standalone.
		obj, _ := fleet.leaseStore.ReadLease(context.Background(), fleetLeaseKey)
		obj.Data.HolderIdentity = deactivatedLeaseHolder
		obj.Data.RenewTime = deactivatedLeaseRenewTime
		_, _ = fleet.leaseStore.UpdateLease(context.Background(), fleetLeaseKey, obj)
		cc := a.configConnector()
		_ = deactivateConfigConnector(cc)
		_ = a.backend.Update(context.Background(), cc)
		fleet.settle()

		out, err := fleet.deactivate("cluster-a")
		if err != nil {
			t.Fatalf("deactivate failed: %v\noutput:\n%s", err, out)
		}
		fleet.requireDeactivatedState(a, b)
	})

	t.Run("row 4a: lease names another cluster Z in --contexts", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		_, err := fleet.deactivate("cluster-b")
		requireErrorContains(t, err,
			`cluster "cluster-a" is the leader`,
			`config-connector ha failover --target-cluster=cluster-b`,
			`--standalone-cluster=cluster-a`,
		)
	})

	t.Run("row 4b: lease names another cluster Z NOT in --contexts", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")
		_, err := fleet.deactivateOn(context.Background(), []*clusterHandle{b.handle}, "cluster-b")
		requireErrorContains(t, err, a.identity(), "not in --contexts")
	})

	t.Run("row 5a: lease closed for another cluster Z", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		a := fleet.cluster("cluster-a")
		obj, _ := fleet.leaseStore.ReadLease(context.Background(), fleetLeaseKey)
		obj.Data.HolderIdentity = closedLeaseHolder(a.identity())
		obj.Data.RenewTime = deactivatedLeaseRenewTime
		_, _ = fleet.leaseStore.UpdateLease(context.Background(), fleetLeaseKey, obj)

		_, err := fleet.deactivate("cluster-b")
		requireErrorContains(t, err, `multi-cluster HA is already deactivated, with cluster "cluster-a" reconciling on its own`)
	})

	t.Run("row 5b: another cluster Z in --contexts is standalone", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		a := fleet.cluster("cluster-a")
		cc := a.configConnector()
		_ = deactivateConfigConnector(cc)
		_ = a.backend.Update(context.Background(), cc)

		_, err := fleet.deactivate("cluster-b")
		requireErrorContains(t, err, `multi-cluster HA is already deactivated, with cluster "cluster-a" reconciling on its own`)
	})

	t.Run("row 6: lease closed without identity and no cluster in --contexts is standalone", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		obj, _ := fleet.leaseStore.ReadLease(context.Background(), fleetLeaseKey)
		obj.Data.HolderIdentity = deactivatedLeaseHolder
		obj.Data.RenewTime = deactivatedLeaseRenewTime
		_, _ = fleet.leaseStore.UpdateLease(context.Background(), fleetLeaseKey, obj)

		_, err := fleet.deactivate("cluster-a")
		requireErrorContains(t, err, "cannot tell for which cluster", "--contexts")
	})

	t.Run("row 7a: no lease object", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		_ = fleet.leaseStore.DeleteLease(context.Background(), fleetLeaseKey)

		_, err := fleet.deactivate("cluster-a")
		requireErrorContains(t, err, "does not exist", "config-connector ha status")
	})

	t.Run("row 7b: X has neither lease nor annotation", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		a := fleet.cluster("cluster-a")
		cc := a.configConnector()
		cc.Spec.Experiments = nil
		_ = a.backend.Update(context.Background(), cc)

		_, err := fleet.deactivate("cluster-a")
		requireErrorContains(t, err, deactivatedLeaseAnnotation, "config-connector ha status")
	})
}

// Test 9: --dry-run changes nothing on any cluster and in the store.
func TestDeactivateDryRunChangesNothing(t *testing.T) {
	for _, mode := range bothFleetModes {
		t.Run(mode.name, func(t *testing.T) {
			fleet := newDeactivateFleet(t, mode, "cluster-a", "cluster-b")
			a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")

			wrapped := &hookLeaseStorage{Storage: fleet.leaseStore}
			out, err := fleet.deactivate("cluster-a", func(opts *deactivateOptions) {
				opts.dryRun = true
				opts.newLeaseStore = func(ctx context.Context, bucket string) (mclstorage.Storage, func() error, error) {
					return wrapped, nil, nil
				}
			})
			if err != nil {
				t.Fatalf("dry-run failed: %v\noutput:\n%s", err, out)
			}

			if wrapped.writes != 0 {
				t.Errorf("dry-run wrote to lease store %d times, want 0", wrapped.writes)
			}
			if len(fleet.patches) != 0 {
				t.Errorf("dry-run made %d patches, want 0: %v", len(fleet.patches), fleet.patches)
			}
			if multiClusterLeaseSpec(a.configConnector()) == nil {
				t.Errorf("dry-run modified cluster-a's ConfigConnector")
			}
			if _, ok := a.configConnector().Annotations[deactivatedLeaseAnnotation]; ok {
				t.Errorf("dry-run added deactivated annotation to cluster-a")
			}
			for _, sts := range b.managers() {
				if *sts.Spec.Replicas != 1 {
					t.Errorf("dry-run scaled cluster-b StatefulSet %s to %d", sts.Name, *sts.Spec.Replicas)
				}
			}
			if deploymentReplicas(b.mclDeployment()) != 1 {
				t.Errorf("dry-run scaled cluster-b MCL Deployment")
			}

			// Every step line in dry-run starts with [dry-run] and says "would".
			foundDryRun := false
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if strings.HasPrefix(line, "Leader:") || strings.HasPrefix(line, "Lease:") {
					continue
				}
				foundDryRun = true
				if !strings.HasPrefix(line, "[dry-run] ") {
					t.Errorf("dry-run line does not start with [dry-run]: %q", line)
				}
				if !strings.Contains(line, "would") {
					t.Errorf("dry-run line does not contain \"would\": %q", line)
				}
			}
			if !foundDryRun {
				t.Errorf("expected [dry-run] plan lines in output:\n%s", out)
			}
		})
	}
}

// Test 10: One context: --contexts=X --standalone-cluster=X.
func TestDeactivateSingleContext(t *testing.T) {
	for _, mode := range bothFleetModes {
		t.Run(mode.name, func(t *testing.T) {
			fleet := newDeactivateFleet(t, mode, "cluster-a")
			a := fleet.cluster("cluster-a")
			fleet.requireLeader(a)

			out, err := fleet.deactivate("cluster-a")
			if err != nil {
				t.Fatalf("single-context deactivate failed: %v\noutput:\n%s", err, out)
			}
			fleet.requireDeactivatedState(a)
		})
	}
}

// Test 11: A parked manager is started by hand afterwards: it does not lead.
func TestDeactivateParkedManagerStartedByHandDoesNotLead(t *testing.T) {
	for _, mode := range bothFleetModes {
		t.Run(mode.name, func(t *testing.T) {
			fleet := newDeactivateFleet(t, mode, "cluster-a", "cluster-b")
			a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")

			if out, err := fleet.deactivate("cluster-a"); err != nil {
				t.Fatalf("deactivate failed: %v\noutput:\n%s", err, out)
			}
			fleet.requireDeactivatedState(a, b)

			// Start cluster-b's managers AND its MCL controller by hand.
			for _, sts := range b.managers() {
				stsCopy := sts
				b.scaleManager(&stsCopy, 1)
			}
			dep := b.mclDeployment()
			one := int32(1)
			dep.Spec.Replicas = &one
			if err := b.backend.Update(context.Background(), dep); err != nil {
				t.Fatalf("scaling cluster-b MCL Deployment back up: %v", err)
			}

			// Run for many lease expirations.
			for i := 0; i < fleet.takeOverSteps*10; i++ {
				fleet.step()
				if b.isReconciling() {
					t.Fatalf("restarted parked manager on cluster-b started reconciling at step %d", fleet.steps)
				}
			}
			if !b.running() {
				t.Fatalf("expected cluster-b's manager pod to be running")
			}
			if got := fleet.reconcilingClusters(); !slices.Equal(got, []string{"cluster-a"}) {
				t.Fatalf("reconciling clusters = %v, want [cluster-a]", got)
			}
		})
	}
}

// Test 12: A StatefulSet at 0 replicas without an annotation on another cluster is left alone and reported.
func TestDeactivateAlreadyZeroStatefulSetLeftAloneAndReported(t *testing.T) {
	for _, mode := range bothFleetModes {
		t.Run(mode.name, func(t *testing.T) {
			fleet := newDeactivateFleet(t, mode, "cluster-a", "cluster-b")
			a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")

			// Scale one StatefulSet on cluster-b to 0 without any annotation.
			managers := b.managers()
			zeroTarget := managers[0]
			b.scaleManager(&zeroTarget, 0)
			fleet.settle()

			out, err := fleet.deactivate("cluster-a")
			if err != nil {
				t.Fatalf("deactivate failed: %v\noutput:\n%s", err, out)
			}

			sts := b.manager(zeroTarget.Namespace, zeroTarget.Name)
			if *sts.Spec.Replicas != 0 {
				t.Errorf("expected %s/%s to stay at 0 replicas, got %d", sts.Namespace, sts.Name, *sts.Spec.Replicas)
			}
			if _, ok := sts.Annotations[deactivatedReplicasAnnotation]; ok {
				t.Errorf("expected already-0 StatefulSet %s/%s to have no %s annotation", sts.Namespace, sts.Name, deactivatedReplicasAnnotation)
			}
			for _, want := range []string{
				zeroTarget.Namespace + "/" + zeroTarget.Name,
				"ha activate will not start it",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}

			// Other managers on cluster-b (in namespaced mode) ARE annotated.
			for _, m := range managers[1:] {
				got := b.manager(m.Namespace, m.Name)
				if got.Annotations[deactivatedReplicasAnnotation] != "1" {
					t.Errorf("expected running StatefulSet %s/%s to be annotated with 1, got %q", m.Namespace, m.Name, got.Annotations[deactivatedReplicasAnnotation])
				}
			}
			if !a.isStandaloneReconciling() {
				t.Errorf("cluster-a is not reconciling standalone")
			}
		})
	}
}

func TestDeactivatePreflightRefusals(t *testing.T) {
	t.Run("interrupted ha failover on any named cluster", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		b := fleet.cluster("cluster-b")
		b.stopManagerAsFailoverDoes(fleetLeaseNamespace, managerComponent)
		fleet.settle()

		_, err := fleet.deactivate("cluster-a")
		requireErrorContains(t, err, `an interrupted "ha failover" left managers stopped on cluster "cluster-b"; run "config-connector ha failover" again first`)
	})

	t.Run("X manager scaled to 0", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		fleet.takeOverSteps = 100 // keep cluster-a as the lease holder while Step 0 inspects it
		a := fleet.cluster("cluster-a")
		a.scaleManager(a.manager(fleetLeaseNamespace, managerComponent), 0)

		_, err := fleet.deactivate("cluster-a")
		requireErrorContains(t, err, `cluster "cluster-a"`, "scaled to 0")
	})

	t.Run("bucket mismatch across clusters", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		b := fleet.cluster("cluster-b")
		dep := b.mclDeployment()
		dep.Spec.Template.Spec.Containers[0].Args = []string{"--gcs-bucket=other-bucket"}
		if err := b.backend.Update(context.Background(), dep); err != nil {
			t.Fatalf("updating cluster-b MCL Deployment: %v", err)
		}

		_, err := fleet.deactivate("cluster-a")
		requireErrorContains(t, err, "other-bucket", fleetLeaseBucket)
	})

	t.Run("server-side dry-run catches missing permission before touching lease", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		b := fleet.cluster("cluster-b")
		b.rejectPatch = func(obj client.Object, patch string) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "statefulsets"}, obj.GetName(), errors.New("forbidden"))
		}

		wrapped := &hookLeaseStorage{Storage: fleet.leaseStore}
		_, err := fleet.deactivate("cluster-a", func(opts *deactivateOptions) {
			opts.newLeaseStore = func(ctx context.Context, bucket string) (mclstorage.Storage, func() error, error) {
				return wrapped, nil, nil
			}
		})
		requireErrorContains(t, err, "dry-run patch", "nothing was changed", "forbidden")
		if wrapped.writes != 0 {
			t.Errorf("expected 0 writes to lease store when dry-run fails, got %d", wrapped.writes)
		}
	})

	t.Run("GCS permission error names bucket, object, and says nothing was changed", func(t *testing.T) {
		fleet := newHAFleet(t, "cluster-a", "cluster-b")
		for _, op := range []string{"read", "write"} {
			t.Run(op, func(t *testing.T) {
				permErr := &googleapi.Error{Code: http.StatusForbidden, Message: "storage.objects.update denied"}
				wrapped := &hookLeaseStorage{
					Storage: fleet.leaseStore,
					onRead: func(ctx context.Context, key string) (*mclstorage.LeaseObject, error, bool) {
						if op == "read" {
							return nil, permErr, true
						}
						return nil, nil, false
					},
					onUpdate: func(ctx context.Context, key string, obj *mclstorage.LeaseObject) (*mclstorage.LeaseObject, error, bool) {
						if op == "write" {
							return nil, permErr, true
						}
						return nil, nil, false
					},
				}
				_, err := fleet.deactivate("cluster-a", func(opts *deactivateOptions) {
					opts.newLeaseStore = func(ctx context.Context, bucket string) (mclstorage.Storage, func() error, error) {
						return wrapped, nil, nil
					}
				})
				requireErrorContains(t, err, fleetLeaseBucket, "gs://test-lease-bucket/leases/cnrm-system/kcc-multicluster-lease", "nothing was changed")
			})
		}
	})
}

func TestDeactivateStep3PartialFailureContinuesAndExitsNonZero(t *testing.T) {
	fleet := newHAFleet(t, "cluster-a", "cluster-b", "cluster-c")
	a, b, c := fleet.cluster("cluster-a"), fleet.cluster("cluster-b"), fleet.cluster("cluster-c")

	// Allow Step 0 dry-run on cluster-b, fail real Step 3 patch on cluster-b.
	bPatches := 0
	b.rejectPatch = func(obj client.Object, patch string) error {
		if _, ok := obj.(*appsv1.StatefulSet); ok {
			bPatches++
			if bPatches >= 2 { // 1st is Step 0 DryRunAll
				return errors.New("cluster-b unreachable")
			}
		}
		return nil
	}

	out, err := fleet.deactivate("cluster-a")
	requireErrorContains(t, err,
		`multi-cluster HA is deactivated, but cluster "cluster-b" is not stopped`,
		"cluster-b unreachable",
		"It cannot lead, because the lease is closed. Run the command again.",
	)

	// cluster-c WAS parked and cluster-a IS standalone reconciling despite cluster-b failing.
	fleet.requireDeactivatedState(a, c)
	if !b.running() {
		t.Errorf("expected cluster-b to still be unparked after its patch failed")
	}
	if !strings.Contains(out, `cluster "cluster-a": reconciles without leader election`) {
		t.Errorf("expected Step 4 to complete for cluster-a:\n%s", out)
	}
}

func TestDeactivateStep4DeletesStuckManagerPod(t *testing.T) {
	fleet := newHAFleet(t, "cluster-a", "cluster-b")
	a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")
	// Keep cluster-a's manager from exiting on its own after losing the lease.
	a.stuckLeader = true

	podBefore, ok := a.getPod(a.manager(fleetLeaseNamespace, managerComponent))
	if !ok {
		t.Fatalf("expected cluster-a to have a manager pod")
	}

	out, err := fleet.deactivate("cluster-a", func(opts *deactivateOptions) {
		opts.podDeleteDelay = 5 * time.Millisecond
	})
	if err != nil {
		t.Fatalf("deactivate failed with stuck leader pod: %v\noutput:\n%s", err, out)
	}

	podAfter, ok := a.getPod(a.manager(fleetLeaseNamespace, managerComponent))
	if !ok {
		t.Fatalf("expected cluster-a to have a recreated manager pod")
	}
	if podAfter.UID == podBefore.UID {
		t.Fatalf("expected stuck pod to be deleted and recreated with a new UID, still %s", podAfter.UID)
	}
	fleet.requireDeactivatedState(a, b)
}

func TestDeactivateInterruptBetweenStep1AndStep2FinishesStep2(t *testing.T) {
	fleet := newHAFleet(t, "cluster-a", "cluster-b")
	a := fleet.cluster("cluster-a")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel ctx right after Step 1 completes (simulating Ctrl-C between Steps 1 and 2).
	_, err := fleet.deactivateOn(ctx, fleet.handles(), "cluster-a", func(opts *deactivateOptions) {
		opts.onAfterStep = func(step string) error {
			if step == "step1" {
				cancel()
			}
			return nil
		}
	})
	requireErrorContains(t, err, "interrupted")

	// Step 2 MUST still have completed so cluster-a is standalone (never closed lease + leader election).
	if multiClusterLeaseSpec(a.configConnector()) != nil {
		t.Fatalf("expected Step 2 to finish despite Ctrl-C after Step 1")
	}
	if _, ok := a.configConnector().Annotations[deactivatedLeaseAnnotation]; !ok {
		t.Fatalf("expected cluster-a to have %s annotation", deactivatedLeaseAnnotation)
	}
}

func TestDeactivateExtractGCSBucketArg(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "equals form", args: []string{"--gcs-bucket=my-bucket"}, want: "my-bucket"},
		{name: "single dash equals form", args: []string{"-gcs-bucket=gs://my-bucket/"}, want: "my-bucket"},
		{name: "separate arg form", args: []string{"--gcs-bucket", "my-bucket"}, want: "my-bucket"},
		{name: "missing", args: []string{"--other=value"}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dep := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "manager", Args: tc.args}},
						},
					},
				},
			}
			if got := extractGCSBucketArg(dep); got != tc.want {
				t.Fatalf("extractGCSBucketArg(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestDeactivateNewGCSLeaseStoreUsesGcloudToken(t *testing.T) {
	var called string
	runner := &fakeTokenRunner{
		out: []byte("ya29.fake-token\n"),
		onOutput: func(name string, args ...string) {
			called = name + " " + strings.Join(args, " ")
		},
	}
	store, closeFn, err := newGCSLeaseStore(context.Background(), runner, "my-bucket")
	if err != nil {
		t.Fatalf("newGCSLeaseStore failed: %v", err)
	}
	if closeFn != nil {
		_ = closeFn()
	}
	if store == nil {
		t.Fatalf("expected non-nil store")
	}
	if want := "gcloud auth print-access-token"; called != want {
		t.Fatalf("runner.output called %q, want %q", called, want)
	}

	// Ensure deactivateConfigConnector preserves other CCExperiments fields and serializes JSON cleanly.
	cc := newConfigConnectorWithMCL("kcc-multicluster-lease", "cnrm-system", "cluster-a")
	cc.Spec.Experiments.MaxConcurrentReconcilesOverrides = map[string]int{"StorageBucket.storage.cnrm.cloud.google.com": 10}
	if err := deactivateConfigConnector(cc); err != nil {
		t.Fatalf("deactivateConfigConnector: %v", err)
	}
	if cc.Spec.Experiments == nil || cc.Spec.Experiments.MultiClusterLease != nil || cc.Spec.Experiments.MaxConcurrentReconcilesOverrides["StorageBucket.storage.cnrm.cloud.google.com"] != 10 {
		t.Fatalf("unexpected Experiments after deactivateConfigConnector: %+v", cc.Spec.Experiments)
	}
	var parsed operatorv1beta1.MultiClusterLeaseSpec
	if err := json.Unmarshal([]byte(cc.Annotations[deactivatedLeaseAnnotation]), &parsed); err != nil {
		t.Fatalf("unmarshaling saved annotation: %v", err)
	}
	if parsed.LeaseName != "kcc-multicluster-lease" {
		t.Fatalf("parsed leaseName = %q, want kcc-multicluster-lease", parsed.LeaseName)
	}
}

type fakeTokenRunner struct {
	out      []byte
	err      error
	onOutput func(name string, args ...string)
}

func (r *fakeTokenRunner) run(ctx context.Context, name string, args ...string) error {
	return fmt.Errorf("unexpected run(%s %v)", name, args)
}

func (r *fakeTokenRunner) output(ctx context.Context, name string, args ...string) ([]byte, error) {
	if r.onOutput != nil {
		r.onOutput(name, args...)
	}
	return r.out, r.err
}

func TestDeactivateSuspendsStandaloneUnsuspendedSyncer(t *testing.T) {
	fleet := newHAFleet(t, "cluster-a", "cluster-b")
	a, b := fleet.cluster("cluster-a"), fleet.cluster("cluster-b")
	// Prevent cluster-a's manager from overwriting the syncer back to suspended before deactivate runs.
	a.abandonedSyncers = true
	syncer := &syncerv1alpha1.KRMSyncer{}
	if err := a.backend.Get(context.Background(), types.NamespacedName{Namespace: fleetLeaseNamespace, Name: fleetLeaseName}, syncer); err != nil {
		t.Fatalf("getting cluster-a KRMSyncer: %v", err)
	}
	syncer.Spec.Suspend = false
	if err := a.backend.Update(context.Background(), syncer); err != nil {
		t.Fatalf("unsuspending cluster-a KRMSyncer: %v", err)
	}

	out, err := fleet.deactivate("cluster-a")
	if err != nil {
		t.Fatalf("deactivate failed: %v\noutput:\n%s", err, out)
	}
	fleet.requireDeactivatedState(a, b)
}
