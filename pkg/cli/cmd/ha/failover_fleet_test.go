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
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	operatorv1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/operator/pkg/apis/core/v1beta1"
	"github.com/GoogleCloudPlatform/k8s-config-connector/pkg/cli/kubecli"
	syncerv1alpha1 "github.com/gke-labs/kube-etl/syncer/api/v1alpha1"
	mclv1alpha1 "github.com/gke-labs/multicluster-leader-election/api/v1alpha1"
	mclstorage "github.com/gke-labs/multicluster-leader-election/pkg/storage"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	fleetLeaseName      = "kcc-multicluster-lease"
	fleetLeaseNamespace = "cnrm-system"
	fleetLeaseKey       = fleetLeaseNamespace + "/" + fleetLeaseName
	fleetLeaseBucket    = "test-lease-bucket"

	managerComponentLabel = "cnrm.cloud.google.com/component"
	managerComponent      = "cnrm-controller-manager"
	scopedNamespaceLabel  = "cnrm.cloud.google.com/scoped-namespace"
)

// haFleet stands in for what runs on the clusters of an HA group while HA commands work on them:
//   - the StatefulSet and Deployment controllers and the kubelet, which stop and start pods;
//   - the shared lease store (storage.NewFakeStorage) and the MCL controllers, which renew or
//     acquire the lease and report its state in every cluster's MultiClusterLease;
//   - Config Connector's managers, which read ConfigConnector on startup (running standalone when
//     spec.experiments.multiClusterLease is absent, or with leader election otherwise), exit and
//     restart on leadership loss, and keep their KRMSyncers pointed at the leader.
//
// The fleet moves one step each time a command calls the API of one of its clusters, so the tests
// need no sleeps. It asserts on every step that managers of two clusters never reconcile at once.
type haFleet struct {
	t        *testing.T
	clusters []*fleetCluster

	// leaseStore is the shared fake GCS lease store used by the MCL controllers and ha deactivate.
	leaseStore  mclstorage.Storage
	leaseBucket string

	// holder is the cluster that holds the lease (nil if closed or held by an external cluster).
	holder *fleetCluster
	// holders are the clusters that have held the lease, in order.
	holders []string
	// renewals counts how often the lease was renewed or taken.
	renewals int
	// leaderless counts the steps since the holder's managers stopped.
	leaderless int

	// takeOverSteps is how many steps the lease takes to expire once the holder's managers have
	// stopped. The first cluster that asks for it then takes it.
	takeOverSteps int
	// podStopSteps is how many steps a manager's or MCL controller's pod takes to stop.
	podStopSteps int
	// leaseLossSteps is how many steps a leading manager takes to exit and restart after losing
	// the lease.
	leaseLossSteps int

	nextUID int
	steps   int
	// onStep, if set, runs at the start of every step.
	onStep func()
	// patches are non-dry-run patches, in order.
	patches []fleetPatch
}

// fleetPatch is one write of failover, and what the fleet looked like when it was made.
type fleetPatch struct {
	cluster    string
	object     string
	replicas   int32
	annotation string
	// holder is the cluster that held the lease.
	holder string
	// running are the clusters that had a running manager.
	running string
}

func (p fleetPatch) String() string {
	if p.replicas == 0 {
		return fmt.Sprintf("stop %s %s (annotation %q) while %s leads and %s run", p.cluster, p.object, p.annotation, p.holder, p.running)
	}
	return fmt.Sprintf("start %s %s (replicas %d, annotation %q) while %s leads", p.cluster, p.object, p.replicas, p.annotation, p.holder)
}

// fleetCluster is one cluster of an haFleet.
type fleetCluster struct {
	fleet *haFleet
	name  string
	gke   operatorv1beta1.GKECluster

	// backend holds the cluster's objects. The fleet uses it directly.
	backend client.WithWatch
	// handle is how failover reaches the cluster. Each call through it moves the fleet one step.
	handle *clusterHandle

	// cannotLead keeps the cluster from taking the lease, as when its MCL controller can't
	// write the lease.
	cannotLead bool
	// stillAsks lets the cluster take an expired lease once while its managers are stopped. Its
	// MCL controller does that for 30 seconds after the managers are gone. Nothing renews a
	// lease that is taken that way.
	stillAsks bool
	// staleLease stops the updates of the cluster's MultiClusterLease, as when its MCL
	// controller has stopped.
	staleLease bool
	// noPods keeps the pods of its managers from starting, as when they can't be scheduled.
	noPods bool
	// stuckPods keeps the pods of its managers from stopping.
	stuckPods bool
	// unreadyPods keeps the pods of its managers from becoming ready.
	unreadyPods bool
	// abandonedSyncers stops the updates of its KRMSyncers, as when its managers can't write
	// them.
	abandonedSyncers bool
	// noSyncer stops the updates of the status of its KRMSyncers, as when its syncer has
	// stopped.
	noSyncer bool
	// syncFailures is what its syncer reports for a KRMSyncer that pulls and doesn't sync, by the
	// name of the KRMSyncer. Without an entry, the syncer reports that the KRMSyncer syncs.
	syncFailures map[string]fleetSyncFailure
	// rejectRead, if set, can fail a read of failover from the cluster. It gets the object or the
	// list that failover asks for.
	rejectRead func(ctx context.Context, obj runtime.Object) error
	// rejectPatch, if set, can fail a write of a command to the cluster. It gets the object and
	// the patch.
	rejectPatch func(obj client.Object, patch string) error
	// rejectUpdate, if set, can fail an Update of a command to the cluster.
	rejectUpdate func(obj client.Object) error
	// rejectDelete, if set, can fail a Delete of a command to the cluster.
	rejectDelete func(obj client.Object) error
	// stuckLeader keeps a leading manager from exiting on its own after losing the lease, so tests
	// can exercise Step 4's stale-pod deletion.
	stuckLeader bool

	// stopping counts, for each manager scaled to 0, the steps since its pod was told to stop.
	stopping map[types.NamespacedName]int
	// mclStopping counts steps since the MCL Deployment was scaled to 0.
	mclStopping int
	// podModes tracks the runtime mode (standalone vs leader-election, leading, reconciling,
	// restart count) of each manager pod currently running on this cluster.
	podModes map[types.NamespacedName]*fleetPodState
}

type fleetPodState struct {
	uid            types.UID
	standalone     bool
	leading        bool
	reconciling    bool
	restartCount   int32
	lostLeaseSteps int
}

func (c *fleetCluster) identity() string {
	return c.gke.ResourceName()
}

// newHAFleet returns a fleet of clusters in cluster mode. Each has one manager and an MCL
// controller Deployment, and the first one is the leader. The clusters' objects are in fake clients.
func newHAFleet(t *testing.T, names ...string) *haFleet {
	t.Helper()
	scheme, err := newHAScheme()
	if err != nil {
		t.Fatalf("newHAScheme() failed: %v", err)
	}
	fleet := newEmptyHAFleet(t)
	for _, name := range names {
		backend := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&appsv1.StatefulSet{}, &appsv1.Deployment{}, &corev1.Pod{}, &mclv1alpha1.MultiClusterLease{}, &syncerv1alpha1.KRMSyncer{}).
			Build()
		c := fleet.addCluster(name, backend)
		c.create(newConfigConnectorWithMCL(fleetLeaseName, fleetLeaseNamespace, name))
		c.create(&mclv1alpha1.MultiClusterLease{ObjectMeta: metav1.ObjectMeta{Namespace: fleetLeaseNamespace, Name: fleetLeaseName}})
		c.create(newManagerStatefulSet(fleetLeaseNamespace, managerComponent, ""))
		c.create(newMCLDeployment(fleet.leaseBucket))
	}
	fleet.settle()
	return fleet
}

func newEmptyHAFleet(t *testing.T) *haFleet {
	return &haFleet{
		t:              t,
		leaseStore:     mclstorage.NewFakeStorage(),
		leaseBucket:    fleetLeaseBucket,
		takeOverSteps:  6,
		podStopSteps:   3,
		leaseLossSteps: 5,
	}
}

// addCluster adds a cluster whose objects are behind backend. The first cluster is the leader.
func (f *haFleet) addCluster(name string, backend client.WithWatch) *fleetCluster {
	c := &fleetCluster{
		fleet:    f,
		name:     name,
		gke:      operatorv1beta1.GKECluster{Project: "test-project", Location: "us-central1", Name: name},
		backend:  backend,
		stopping: map[types.NamespacedName]int{},
		podModes: map[types.NamespacedName]*fleetPodState{},
	}
	// A request fails once its context is done, as a request to an API server does.
	stepping := interceptor.NewClient(backend, interceptor.Funcs{
		Get: func(ctx context.Context, backend client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			f.step()
			if err := c.failRead(ctx, obj); err != nil {
				return err
			}
			return backend.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, backend client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			f.step()
			if err := c.failRead(ctx, list); err != nil {
				return err
			}
			return backend.List(ctx, list, opts...)
		},
		Patch: func(ctx context.Context, backend client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			dryRun := isDryRunPatch(opts...)
			if !dryRun {
				f.step()
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if c.rejectPatch != nil {
				data, err := patch.Data(obj)
				if err != nil {
					return err
				}
				if err := c.rejectPatch(obj, string(data)); err != nil {
					return err
				}
			}
			if err := backend.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			if !dryRun {
				f.recordPatch(c, obj)
			}
			return nil
		},
		Update: func(ctx context.Context, backend client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			dryRun := isDryRunUpdate(opts...)
			if !dryRun {
				f.step()
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if c.rejectUpdate != nil {
				if err := c.rejectUpdate(obj); err != nil {
					return err
				}
			}
			return backend.Update(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, backend client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			f.step()
			if err := ctx.Err(); err != nil {
				return err
			}
			if c.rejectDelete != nil {
				if err := c.rejectDelete(obj); err != nil {
					return err
				}
			}
			return backend.Delete(ctx, obj, opts...)
		},
	})
	c.handle = &clusterHandle{contextName: name, client: &kubecli.Client{Client: stepping}}
	f.clusters = append(f.clusters, c)
	if f.holder == nil {
		f.giveLease(c)
	}
	return c
}

func isDryRunPatch(opts ...client.PatchOption) bool {
	po := &client.PatchOptions{}
	for _, opt := range opts {
		opt.ApplyToPatch(po)
	}
	for _, dr := range po.DryRun {
		if dr == metav1.DryRunAll {
			return true
		}
	}
	return false
}

func isDryRunUpdate(opts ...client.UpdateOption) bool {
	uo := &client.UpdateOptions{}
	for _, opt := range opts {
		opt.ApplyToUpdate(uo)
	}
	for _, dr := range uo.DryRun {
		if dr == metav1.DryRunAll {
			return true
		}
	}
	return false
}

func (c *fleetCluster) failRead(ctx context.Context, obj runtime.Object) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.rejectRead != nil {
		return c.rejectRead(ctx, obj)
	}
	return nil
}

func (f *haFleet) recordPatch(c *fleetCluster, obj client.Object) {
	holder := ""
	if f.holder != nil {
		holder = f.holder.name
	}
	annotation := obj.GetAnnotations()[failoverReplicasAnnotation]
	if annotation == "" {
		annotation = obj.GetAnnotations()[deactivatedReplicasAnnotation]
	}
	p := fleetPatch{
		cluster:    c.name,
		object:     obj.GetNamespace() + "/" + obj.GetName(),
		annotation: annotation,
		holder:     holder,
		running:    strings.Join(f.runningClusters(), ","),
	}
	if sts, ok := obj.(*appsv1.StatefulSet); ok && sts.Spec.Replicas != nil {
		p.replicas = *sts.Spec.Replicas
	} else if dep, ok := obj.(*appsv1.Deployment); ok && dep.Spec.Replicas != nil {
		p.replicas = *dep.Spec.Replicas
	}
	f.patches = append(f.patches, p)
}

func (f *haFleet) cluster(name string) *fleetCluster {
	f.t.Helper()
	for _, c := range f.clusters {
		if c.name == name {
			return c
		}
	}
	f.t.Fatalf("the fleet has no cluster %q", name)
	return nil
}

func (f *haFleet) handles() []*clusterHandle {
	handles := make([]*clusterHandle, 0, len(f.clusters))
	for _, c := range f.clusters {
		handles = append(handles, c.handle)
	}
	return handles
}

func (f *haFleet) runningClusters() []string {
	var running []string
	for _, c := range f.clusters {
		if c.running() {
			running = append(running, c.name)
		}
	}
	return running
}

// settle runs the fleet until its controllers have caught up with the objects, as they would
// have by the time someone runs failover.
func (f *haFleet) settle() {
	for i := 0; i < f.takeOverSteps+f.podStopSteps+2; i++ {
		f.step()
	}
}

// step moves every component of the fleet forward once.
func (f *haFleet) step() {
	f.t.Helper()
	f.steps++
	if f.onStep != nil {
		f.onStep()
	}
	for _, c := range f.clusters {
		c.runPods()
		c.runDeployments()
	}
	f.renewOrTakeLease()
	for _, c := range f.clusters {
		c.updateManagerModes()
		c.reportLease()
		c.maintainSyncers()
		c.reportSync()
	}
	f.assertNeverTwoReconciling()
}

func (f *haFleet) clusterByIdentity(identity string) *fleetCluster {
	for _, c := range f.clusters {
		if c.identity() == identity {
			return c
		}
	}
	return nil
}

// renewOrTakeLease is the lease itself, backed by f.leaseStore: the holder keeps it for as long as
// its MCL controller runs and one of its leader-election managers runs. After that it expires
// (unless its renewTime is in the future, as when closed), and the first cluster that asks for it
// takes it.
func (f *haFleet) renewOrTakeLease() {
	ctx := context.Background()
	obj, err := f.leaseStore.ReadLease(ctx, fleetLeaseKey)
	if err != nil {
		if f.leaseStore.IsNotFound(err) {
			f.holder = nil
			return
		}
		f.t.Fatalf("reading fleet lease from store: %v", err)
	}

	holder := f.clusterByIdentity(obj.Data.HolderIdentity)
	f.holder = holder

	if holder != nil && holder.mclRunning() && holder.runningInLeaderElectionMode() {
		f.renewals++
		f.leaderless = 0
		obj.Data.RenewTime = f.renewTimeVal()
		if _, err := f.leaseStore.UpdateLease(ctx, fleetLeaseKey, obj); err != nil {
			f.t.Fatalf("renewing fleet lease in store: %v", err)
		}
		return
	}

	// A closed lease (renewTime 9999-12-31T00:00:00Z) never expires.
	if obj.Data.RenewTime.After(f.renewTimeVal()) {
		f.leaderless = 0
		return
	}

	f.leaderless++
	if f.leaderless < f.takeOverSteps {
		return
	}
	for _, c := range f.clusters {
		if c.cannotLead || !c.mclRunning() || !(c.runningInLeaderElectionMode() || c.stillAsks) {
			continue
		}
		c.stillAsks = false
		f.giveLease(c)
		return
	}
}

// giveLease makes the cluster the holder of the lease in f.leaseStore.
func (f *haFleet) giveLease(c *fleetCluster) {
	ctx := context.Background()
	f.holder = c
	f.holders = append(f.holders, c.name)
	f.renewals++
	f.leaderless = 0

	// A new leader only takes over after the prior leader's RenewDeadline has passed.
	for _, other := range f.clusters {
		if other == c {
			continue
		}
		for _, state := range other.podModes {
			if !state.standalone {
				state.leading = false
				state.reconciling = false
			}
		}
	}

	now := f.renewTimeVal()
	obj, err := f.leaseStore.ReadLease(ctx, fleetLeaseKey)
	switch {
	case err == nil:
		if obj.Data.HolderIdentity != c.identity() && obj.Data.HolderIdentity != "" {
			obj.Data.LeaseTransitions++
		}
		obj.Data.HolderIdentity = c.identity()
		obj.Data.RenewTime = now
		if _, err := f.leaseStore.UpdateLease(ctx, fleetLeaseKey, obj); err != nil {
			f.t.Fatalf("updating fleet lease in store for %q: %v", c.name, err)
		}
	case f.leaseStore.IsNotFound(err):
		data := mclstorage.LeaseData{
			HolderIdentity:   c.identity(),
			RenewTime:        now,
			LeaseTransitions: 1,
		}
		if _, err := f.leaseStore.CreateLease(ctx, fleetLeaseKey, data); err != nil {
			f.t.Fatalf("creating fleet lease in store for %q: %v", c.name, err)
		}
	default:
		f.t.Fatalf("reading fleet lease from store for %q: %v", c.name, err)
	}
}

func (f *haFleet) renewTimeVal() time.Time {
	return time.Date(2026, time.November, 18, 12, 0, 0, 0, time.UTC).Add(time.Duration(f.renewals) * time.Second)
}

func (f *haFleet) renewTime() string {
	return f.renewTimeVal().Format(time.RFC3339)
}

func (f *haFleet) nextPodUID(cluster, stsName string) types.UID {
	f.nextUID++
	return types.UID(fmt.Sprintf("%s-%s-uid-%d", cluster, stsName, f.nextUID))
}

func (f *haFleet) reconcilingClusters() []string {
	var names []string
	for _, c := range f.clusters {
		if c.isReconciling() {
			names = append(names, c.name)
		}
	}
	return names
}

func (f *haFleet) assertNeverTwoReconciling() {
	f.t.Helper()
	if reconciling := f.reconcilingClusters(); len(reconciling) > 1 {
		f.t.Fatalf("invariant violated at step %d: managers of multiple clusters reconcile at the same time: %v", f.steps, reconciling)
	}
}

func (c *fleetCluster) create(obj client.Object) {
	c.fleet.t.Helper()
	if err := c.backend.Create(context.Background(), obj); err != nil {
		c.fleet.t.Fatalf("cluster %q: creating %T %s/%s: %v", c.name, obj, obj.GetNamespace(), obj.GetName(), err)
	}
}

func (c *fleetCluster) managers() []appsv1.StatefulSet {
	c.fleet.t.Helper()
	list := &appsv1.StatefulSetList{}
	if err := c.backend.List(context.Background(), list, client.MatchingLabels{managerComponentLabel: managerComponent}); err != nil {
		c.fleet.t.Fatalf("cluster %q: listing the managers: %v", c.name, err)
	}
	return list.Items
}

func (c *fleetCluster) manager(namespace, name string) *appsv1.StatefulSet {
	c.fleet.t.Helper()
	sts := &appsv1.StatefulSet{}
	if err := c.backend.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, sts); err != nil {
		c.fleet.t.Fatalf("cluster %q: getting StatefulSet %s/%s: %v", c.name, namespace, name, err)
	}
	return sts
}

func managerPodKey(sts *appsv1.StatefulSet) types.NamespacedName {
	return types.NamespacedName{Namespace: sts.Namespace, Name: sts.Name + "-0"}
}

func (c *fleetCluster) getPod(sts *appsv1.StatefulSet) (*corev1.Pod, bool) {
	c.fleet.t.Helper()
	pod := &corev1.Pod{}
	err := c.backend.Get(context.Background(), managerPodKey(sts), pod)
	if err != nil && !apierrors.IsNotFound(err) {
		c.fleet.t.Fatalf("cluster %q: getting pod %s: %v", c.name, managerPodKey(sts), err)
	}
	if err != nil {
		return nil, false
	}
	return pod, true
}

func (c *fleetCluster) hasPod(sts *appsv1.StatefulSet) bool {
	_, ok := c.getPod(sts)
	return ok
}

// running reports whether a manager of the cluster runs. A manager that was scaled to 0 runs
// until its pod is gone.
func (c *fleetCluster) running() bool {
	for _, sts := range c.managers() {
		if c.hasPod(&sts) {
			return true
		}
	}
	return false
}

// runningInLeaderElectionMode reports whether a manager pod of the cluster is currently running
// with leader election enabled (not standalone).
func (c *fleetCluster) runningInLeaderElectionMode() bool {
	for _, sts := range c.managers() {
		if !c.hasPod(&sts) {
			continue
		}
		key := client.ObjectKeyFromObject(&sts)
		state := c.podModes[key]
		if state == nil || !state.standalone {
			return true
		}
	}
	return false
}

// isReconciling reports whether at least one manager pod of the cluster is actively reconciling
// (either leading via the lease or running standalone).
func (c *fleetCluster) isReconciling() bool {
	for _, sts := range c.managers() {
		if !c.hasPod(&sts) {
			continue
		}
		if state := c.podModes[client.ObjectKeyFromObject(&sts)]; state != nil && state.reconciling {
			return true
		}
	}
	return false
}

// isStandaloneReconciling reports whether every manager StatefulSet of the cluster has a running
// pod that is reconciling in standalone mode.
func (c *fleetCluster) isStandaloneReconciling() bool {
	managers := c.managers()
	if len(managers) == 0 {
		return false
	}
	for _, sts := range managers {
		if !c.hasPod(&sts) {
			return false
		}
		state := c.podModes[client.ObjectKeyFromObject(&sts)]
		if state == nil || !state.standalone || !state.reconciling {
			return false
		}
	}
	return true
}

// mclRunning reports whether the cluster's MultiClusterLease controller Deployment has a running
// pod. If no MCL Deployment exists on the cluster, it defaults to true for tests that omit it.
func (c *fleetCluster) mclRunning() bool {
	c.fleet.t.Helper()
	dep := &appsv1.Deployment{}
	err := c.backend.Get(context.Background(), types.NamespacedName{Namespace: manifestSystemNamespace, Name: mclDeploymentName}, dep)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true
		}
		c.fleet.t.Fatalf("cluster %q: getting MCL Deployment: %v", c.name, err)
	}
	if deploymentReplicas(dep) > 0 {
		return true
	}
	return dep.Status.Replicas > 0
}

func (c *fleetCluster) configConnectorHasMCL() bool {
	c.fleet.t.Helper()
	cc := &operatorv1beta1.ConfigConnector{}
	if err := c.backend.Get(context.Background(), types.NamespacedName{Name: configConnectorResourceName}, cc); err != nil {
		if apierrors.IsNotFound(err) {
			return true
		}
		c.fleet.t.Fatalf("cluster %q: getting ConfigConnector: %v", c.name, err)
	}
	return multiClusterLeaseSpec(cc) != nil
}

// runPods is the StatefulSet controller and the kubelet: it starts and stops the pod of each
// manager, reads ConfigConnector on pod startup to pick standalone vs leader-election mode, and
// reports pod and StatefulSet status.
func (c *fleetCluster) runPods() {
	t := c.fleet.t
	t.Helper()
	ctx := context.Background()
	managers := c.managers()
	activeKeys := make(map[types.NamespacedName]bool, len(managers))
	for _, sts := range managers {
		key := client.ObjectKeyFromObject(&sts)
		activeKeys[key] = true
		pod, running := c.getPod(&sts)

		if *sts.Spec.Replicas == 0 {
			if running && !c.stuckPods {
				c.stopping[key]++
				if c.stopping[key] >= c.fleet.podStopSteps {
					delPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: sts.Namespace, Name: managerPodKey(&sts).Name}}
					if err := c.backend.Delete(ctx, delPod, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
						t.Fatalf("cluster %q: deleting pod %s: %v", c.name, managerPodKey(&sts), err)
					}
					delete(c.podModes, key)
					running = false
					pod = nil
				}
			}
			if !running {
				delete(c.podModes, key)
			}
		} else {
			delete(c.stopping, key)
			if !running {
				delete(c.podModes, key)
				if !c.noPods {
					newPod := &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: sts.Namespace,
							Name:      managerPodKey(&sts).Name,
							Labels:    sts.Spec.Template.Labels,
						},
						Spec: sts.Spec.Template.Spec,
					}
					c.create(newPod)
					if newPod.UID == "" {
						newPod.UID = c.fleet.nextPodUID(c.name, sts.Name)
						if err := c.backend.Update(ctx, newPod); err != nil {
							t.Fatalf("cluster %q: setting UID on pod %s: %v", c.name, managerPodKey(&sts), err)
						}
					}
					standalone := !c.configConnectorHasMCL()
					leading := !standalone && c.fleet.holder == c && c.mclRunning()
					c.podModes[key] = &fleetPodState{
						uid:         newPod.UID,
						standalone:  standalone,
						leading:     leading,
						reconciling: standalone || leading,
					}
					pod = newPod
					running = true
				}
			} else if _, tracked := c.podModes[key]; !tracked {
				standalone := !c.configConnectorHasMCL()
				leading := !standalone && c.fleet.holder == c && c.mclRunning()
				c.podModes[key] = &fleetPodState{
					uid:          pod.UID,
					standalone:   standalone,
					leading:      leading,
					reconciling:  standalone || leading,
					restartCount: managerContainerRestartCount(pod),
				}
			}
		}

		if running && pod != nil {
			state := c.podModes[key]
			ready := !c.unreadyPods && *sts.Spec.Replicas > 0
			c.syncPodStatus(pod, ready, state.restartCount)
		}

		status := appsv1.StatefulSetStatus{ObservedGeneration: sts.Generation}
		if running {
			status.Replicas = 1
			if !c.unreadyPods && *sts.Spec.Replicas > 0 {
				status.ReadyReplicas = 1
				status.AvailableReplicas = 1
			}
		}
		if !reflect.DeepEqual(sts.Status, status) {
			sts.Status = status
			if err := c.backend.Status().Update(ctx, &sts); err != nil && !apierrors.IsConflict(err) {
				t.Fatalf("cluster %q: updating the status of StatefulSet %s: %v", c.name, key, err)
			}
		}
	}
	for key := range c.podModes {
		if !activeKeys[key] {
			delete(c.podModes, key)
		}
	}
}

func (c *fleetCluster) syncPodStatus(pod *corev1.Pod, ready bool, restartCount int32) {
	t := c.fleet.t
	t.Helper()
	readyStatus := corev1.ConditionFalse
	if ready {
		readyStatus = corev1.ConditionTrue
	}
	startedAt := metav1.NewTime(time.Date(2026, time.November, 18, 11, 50, 0, 0, time.UTC))
	wantCond := corev1.PodCondition{Type: corev1.PodReady, Status: readyStatus}
	wantCS := corev1.ContainerStatus{
		Name:         "manager",
		Ready:        ready,
		RestartCount: restartCount,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: startedAt},
		},
	}
	needsUpdate := len(pod.Status.Conditions) != 1 || pod.Status.Conditions[0].Type != wantCond.Type || pod.Status.Conditions[0].Status != wantCond.Status ||
		len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].Name != wantCS.Name ||
		pod.Status.ContainerStatuses[0].Ready != wantCS.Ready || pod.Status.ContainerStatuses[0].RestartCount != wantCS.RestartCount
	if !needsUpdate {
		return
	}
	pod.Status.Conditions = []corev1.PodCondition{wantCond}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{wantCS}
	if err := c.backend.Status().Update(context.Background(), pod); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
		t.Fatalf("cluster %q: updating pod status %s/%s: %v", c.name, pod.Namespace, pod.Name, err)
	}
}

// runDeployments updates the status of Deployment cnrm-system/multiclusterlease-controller-manager.
func (c *fleetCluster) runDeployments() {
	t := c.fleet.t
	t.Helper()
	ctx := context.Background()
	dep := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: manifestSystemNamespace, Name: mclDeploymentName}
	if err := c.backend.Get(ctx, key, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		t.Fatalf("cluster %q: getting Deployment %s: %v", c.name, key, err)
	}
	replicas := deploymentReplicas(dep)
	var status appsv1.DeploymentStatus
	if replicas == 0 {
		if dep.Status.Replicas > 0 {
			c.mclStopping++
			if c.mclStopping < c.fleet.podStopSteps {
				return
			}
		}
		status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation}
	} else {
		c.mclStopping = 0
		status = appsv1.DeploymentStatus{
			ObservedGeneration: dep.Generation,
			Replicas:           replicas,
			UpdatedReplicas:    replicas,
			ReadyReplicas:      replicas,
			AvailableReplicas:  replicas,
		}
	}
	if !reflect.DeepEqual(dep.Status, status) {
		dep.Status = status
		if err := c.backend.Status().Update(ctx, dep); err != nil && !apierrors.IsConflict(err) {
			t.Fatalf("cluster %q: updating status of Deployment %s: %v", c.name, key, err)
		}
	}
}

// updateManagerModes updates each running manager pod's leadership / standalone state based on
// the lease in f.leaseStore. A leading manager that loses the lease exits and restarts its
// container, re-reading ConfigConnector on restart.
func (c *fleetCluster) updateManagerModes() {
	t := c.fleet.t
	t.Helper()
	ctx := context.Background()
	leaseObj, err := c.fleet.leaseStore.ReadLease(ctx, fleetLeaseKey)
	if err != nil && !c.fleet.leaseStore.IsNotFound(err) {
		t.Fatalf("reading fleet lease from store: %v", err)
	}

	holdsLease := leaseObj != nil && leaseObj.Data.HolderIdentity == c.identity() && c.mclRunning()
	for _, sts := range c.managers() {
		key := client.ObjectKeyFromObject(&sts)
		pod, running := c.getPod(&sts)
		if !running {
			delete(c.podModes, key)
			continue
		}
		state := c.podModes[key]
		if state == nil {
			continue
		}
		if state.standalone {
			state.leading = false
			state.reconciling = !c.unreadyPods && *sts.Spec.Replicas > 0
			continue
		}
		if state.leading && !holdsLease {
			if !c.stuckLeader {
				state.lostLeaseSteps++
				if state.lostLeaseSteps >= c.fleet.leaseLossSteps {
					// Container exits on losing the lease and restarts, re-reading ConfigConnector.
					state.restartCount++
					state.lostLeaseSteps = 0
					state.standalone = !c.configConnectorHasMCL()
					state.leading = false
					state.reconciling = state.standalone && !c.unreadyPods && *sts.Spec.Replicas > 0
					c.syncPodStatus(pod, !c.unreadyPods && *sts.Spec.Replicas > 0, state.restartCount)
				}
			}
		} else if !state.leading && holdsLease {
			state.leading = true
			state.reconciling = true
			state.lostLeaseSteps = 0
		} else if state.leading && holdsLease {
			state.lostLeaseSteps = 0
		}
	}
}

// reportLease is the cluster's MCL controller: it writes the holder of the lease and its last
// renewal from f.leaseStore to the status of the cluster's MultiClusterLease.
func (c *fleetCluster) reportLease() {
	if c.staleLease || !c.mclRunning() {
		return
	}
	obj, err := c.fleet.leaseStore.ReadLease(context.Background(), fleetLeaseKey)
	if err != nil {
		if c.fleet.leaseStore.IsNotFound(err) {
			return
		}
		c.fleet.t.Fatalf("reading fleet lease from store: %v", err)
	}
	c.setLeaseStatus(obj.Data.HolderIdentity, obj.Data.RenewTime.UTC().Format(time.RFC3339))
}

func (c *fleetCluster) setLeaseStatus(holder, renewTime string) {
	t := c.fleet.t
	t.Helper()
	ctx := context.Background()
	lease := &mclv1alpha1.MultiClusterLease{}
	if err := c.backend.Get(ctx, types.NamespacedName{Namespace: fleetLeaseNamespace, Name: fleetLeaseName}, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		t.Fatalf("cluster %q: getting the MultiClusterLease: %v", c.name, err)
	}
	status := lease.Status.DeepCopy()
	status.GlobalHolderIdentity, status.GlobalRenewTime = nil, nil
	if holder != "" {
		status.GlobalHolderIdentity = &holder
	}
	if renewTime != "" {
		status.GlobalRenewTime = &renewTime
	}
	if reflect.DeepEqual(&lease.Status, status) {
		return
	}
	lease.Status = *status
	if err := c.backend.Status().Update(ctx, lease); err != nil {
		t.Fatalf("cluster %q: updating the status of the MultiClusterLease: %v", c.name, err)
	}
}

// syncerNameOf returns the name of the KRMSyncer that a manager maintains: the lease's in cluster
// mode, with the namespace appended in namespaced mode (SyncerIntegration.getDesiredSyncers in
// pkg/controller/kccmanager).
func syncerNameOf(sts *appsv1.StatefulSet) string {
	if namespace := sts.Labels[scopedNamespaceLabel]; namespace != "" {
		return fleetLeaseName + "-" + namespace
	}
	return fleetLeaseName
}

// maintainSyncers is the cluster's Config Connector managers: each running leader-election manager
// keeps its KRMSyncer naming the leader, suspended on the leader itself and pulling everywhere else.
// Standalone managers do not manage KRMSyncers.
func (c *fleetCluster) maintainSyncers() {
	t := c.fleet.t
	t.Helper()
	if c.abandonedSyncers || !c.syncs() {
		return
	}
	ctx := context.Background()
	leader := c.fleet.holder
	if leader == nil {
		// If the cluster's MultiClusterLease still names a cluster from before the lease closed,
		// a running leader-election standby manager continues keeping its syncer pointed there.
		lease := &mclv1alpha1.MultiClusterLease{}
		if err := c.backend.Get(ctx, types.NamespacedName{Namespace: fleetLeaseNamespace, Name: fleetLeaseName}, lease); err == nil && lease.Status.GlobalHolderIdentity != nil {
			leader = c.fleet.clusterByIdentity(*lease.Status.GlobalHolderIdentity)
		}
	}
	if leader == nil {
		return
	}
	for _, sts := range c.managers() {
		if !c.hasPod(&sts) {
			continue
		}
		if state := c.podModes[client.ObjectKeyFromObject(&sts)]; state != nil && state.standalone {
			continue
		}
		name := syncerNameOf(&sts)
		spec := newSyncerSpec(leader.gke, c == leader)

		syncer := &syncerv1alpha1.KRMSyncer{}
		err := c.backend.Get(ctx, types.NamespacedName{Namespace: fleetLeaseNamespace, Name: name}, syncer)
		switch {
		case apierrors.IsNotFound(err):
			c.create(&syncerv1alpha1.KRMSyncer{ObjectMeta: metav1.ObjectMeta{Namespace: fleetLeaseNamespace, Name: name}, Spec: spec})
		case err != nil:
			t.Fatalf("cluster %q: getting KRMSyncer %s: %v", c.name, name, err)
		case !reflect.DeepEqual(syncer.Spec, spec):
			syncer.Spec = spec
			if err := c.backend.Update(ctx, syncer); err != nil {
				t.Fatalf("cluster %q: updating KRMSyncer %s: %v", c.name, name, err)
			}
		}
	}
}

// fleetSyncFailure is what a syncer reports for a KRMSyncer that doesn't sync: the reason and
// the message of its Ready condition, and the number of objects that fail.
type fleetSyncFailure struct {
	reason        string
	message       string
	failedObjects int32
}

// syncerReady returns the Ready condition that a working syncer reports for a KRMSyncer:
// Suspended if spec.suspend is set, as on the leader, and Syncing otherwise.
func syncerReady(syncer *syncerv1alpha1.KRMSyncer) metav1.Condition {
	ready := metav1.Condition{
		Type:               syncerv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             syncerv1alpha1.ReasonSyncing,
		Message:            "Syncing resources",
		ObservedGeneration: syncer.Generation,
	}
	if syncer.Spec.Suspend {
		ready.Reason, ready.Message = syncerv1alpha1.ReasonSuspended, "Sync is suspended by spec.suspend"
	}
	return ready
}

// reportSync is the cluster's syncer: it reports, in the status of each KRMSyncer, the spec
// that it has processed and whether it syncs.
func (c *fleetCluster) reportSync() {
	t := c.fleet.t
	t.Helper()
	if c.noSyncer {
		return
	}
	for _, syncer := range c.syncers() {
		ready := syncerReady(&syncer)
		var failedObjects int32
		if failure, failing := c.syncFailures[syncer.Name]; failing && !syncer.Spec.Suspend {
			ready.Status, ready.Reason, ready.Message = metav1.ConditionFalse, failure.reason, failure.message
			failedObjects = failure.failedObjects
		}

		status := syncer.Status.DeepCopy()
		status.ObservedGeneration = syncer.Generation
		status.FailedObjects = failedObjects
		meta.SetStatusCondition(&status.Conditions, ready)
		if reflect.DeepEqual(&syncer.Status, status) {
			continue
		}
		syncer.Status = *status
		if err := c.backend.Status().Update(context.Background(), &syncer); err != nil {
			t.Fatalf("cluster %q: updating the status of KRMSyncer %s: %v", c.name, syncer.Name, err)
		}
	}
}

// newSyncerSpec returns the spec of a KRMSyncer that names remote as its remote cluster.
func newSyncerSpec(remote operatorv1beta1.GKECluster, suspend bool) syncerv1alpha1.KRMSyncerSpec {
	return syncerv1alpha1.KRMSyncerSpec{
		Suspend: suspend,
		Mode:    syncerv1alpha1.ModePull,
		Remote: &syncerv1alpha1.RemoteConfig{GKECluster: &syncerv1alpha1.GKECluster{
			Project: remote.Project, Location: remote.Location, Name: remote.Name, Endpoint: syncerv1alpha1.GKEEndpointDefault,
		}},
		Rules: []syncerv1alpha1.ResourceRule{{Group: "*.cnrm.cloud.google.com", Version: "*", Kind: "*", SyncFields: []string{"status"}}},
	}
}

// syncs reports whether resource replication is on in the cluster's ConfigConnector.
func (c *fleetCluster) syncs() bool {
	c.fleet.t.Helper()
	cc := &operatorv1beta1.ConfigConnector{}
	if err := c.backend.Get(context.Background(), types.NamespacedName{Name: configConnectorResourceName}, cc); err != nil {
		c.fleet.t.Fatalf("cluster %q: getting the ConfigConnector: %v", c.name, err)
	}
	mcl := multiClusterLeaseSpec(cc)
	return mcl != nil && mcl.ResourceReplicationMode != "Disabled"
}

func (c *fleetCluster) syncers() []syncerv1alpha1.KRMSyncer {
	c.fleet.t.Helper()
	list := &syncerv1alpha1.KRMSyncerList{}
	if err := c.backend.List(context.Background(), list, client.InNamespace(fleetLeaseNamespace)); err != nil {
		c.fleet.t.Fatalf("cluster %q: listing the KRMSyncers: %v", c.name, err)
	}
	return list.Items
}

// newMCLDeployment returns a MultiClusterLease controller Deployment configured with --gcs-bucket.
func newMCLDeployment(bucket string) *appsv1.Deployment {
	replicas := int32(1)
	labels := map[string]string{"app": "mcl-controller"}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: manifestSystemNamespace,
			Name:      mclDeploymentName,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "manager",
							Image: "gcr.io/gke-release/cnrm/multiclusterlease:1.162.0",
							Args:  []string{"--gcs-bucket=" + bucket},
						},
					},
				},
			},
		},
	}
}

// newManagerStatefulSet returns a StatefulSet of Config Connector's manager with the labels that
// the operator gives it. scopedNamespace is the namespace that a per-namespace manager is for,
// and empty in cluster mode.
func newManagerStatefulSet(namespace, name, scopedNamespace string) *appsv1.StatefulSet {
	labels := map[string]string{
		managerComponentLabel:          managerComponent,
		"cnrm.cloud.google.com/system": "true",
	}
	if scopedNamespace != "" {
		labels[scopedNamespaceLabel] = scopedNamespace
	}
	replicas := int32(1)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   namespace,
			Name:        name,
			Labels:      labels,
			Annotations: map[string]string{"cnrm.cloud.google.com/version": "1.162.0"},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: "cnrm-manager",
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "manager", Image: "gcr.io/gke-release/cnrm/controller:1.162.0"}},
				},
			},
		},
	}
}

// useNamespacedMode replaces the cluster's manager with one manager per namespace, named and
// labeled as the operator does. managerNamespaces says where each one runs: cnrm-system, or a
// namespace of its own.
func (c *fleetCluster) useNamespacedMode(managerNamespaces map[string]string) {
	t := c.fleet.t
	t.Helper()
	ctx := context.Background()
	if err := c.backend.Delete(ctx, c.manager(fleetLeaseNamespace, managerComponent)); err != nil {
		t.Fatalf("cluster %q: deleting the manager: %v", c.name, err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: fleetLeaseNamespace, Name: managerComponent + "-0"}}
	if err := c.backend.Delete(ctx, pod); err != nil {
		t.Fatalf("cluster %q: deleting the manager's pod: %v", c.name, err)
	}
	for _, syncer := range c.syncers() {
		if err := c.backend.Delete(ctx, &syncer); err != nil {
			t.Fatalf("cluster %q: deleting KRMSyncer %s: %v", c.name, syncer.Name, err)
		}
	}
	for namespace, managerNamespace := range managerNamespaces {
		c.create(newManagerStatefulSet(managerNamespace, managerComponent+"-"+namespace, namespace))
	}
}

// stopManagerAsFailoverDoes leaves a manager as an interrupted failover does: scaled to 0, with
// the replicas it had in the annotation.
func (c *fleetCluster) stopManagerAsFailoverDoes(namespace, name string) {
	c.fleet.t.Helper()
	sts := c.manager(namespace, name)
	sts.Annotations[failoverReplicasAnnotation] = "1"
	c.scaleManager(sts, 0)
}

func (c *fleetCluster) scaleManager(sts *appsv1.StatefulSet, replicas int32) {
	c.fleet.t.Helper()
	sts.Spec.Replicas = &replicas
	if err := c.backend.Update(context.Background(), sts); err != nil {
		c.fleet.t.Fatalf("cluster %q: scaling StatefulSet %s/%s: %v", c.name, sts.Namespace, sts.Name, err)
	}
}

// requireManager checks the replicas of a manager, and that failover's annotation is gone.
func (c *fleetCluster) requireManager(namespace, name string, wantReplicas int32) {
	t := c.fleet.t
	t.Helper()
	sts := c.manager(namespace, name)
	if got := *sts.Spec.Replicas; got != wantReplicas {
		t.Errorf("cluster %q: StatefulSet %s/%s has %d replicas, want %d", c.name, namespace, name, got, wantReplicas)
	}
	if value, found := sts.Annotations[failoverReplicasAnnotation]; found {
		t.Errorf("cluster %q: StatefulSet %s/%s still has the annotation %s=%q", c.name, namespace, name, failoverReplicasAnnotation, value)
	}
	if got, want := sts.Annotations["cnrm.cloud.google.com/version"], "1.162.0"; got != want {
		t.Errorf("cluster %q: StatefulSet %s/%s lost its other annotations: version is %q, want %q", c.name, namespace, name, got, want)
	}
}

// requireLeader checks that leader holds the lease, and that every running manager has caught up
// with it: the leader's KRMSyncers are suspended, and the others pull from the leader.
func (f *haFleet) requireLeader(leader *fleetCluster) {
	t := f.t
	t.Helper()
	if f.holder != leader {
		t.Fatalf("cluster %q holds the lease, want cluster %q", f.holder.name, leader.name)
	}
	for _, c := range f.clusters {
		if !c.syncs() {
			continue
		}
		syncers := map[string]syncerv1alpha1.KRMSyncer{}
		for _, syncer := range c.syncers() {
			syncers[syncer.Name] = syncer
		}
		for _, sts := range c.managers() {
			if !c.hasPod(&sts) {
				continue
			}
			syncer, found := syncers[syncerNameOf(&sts)]
			if !found {
				t.Errorf("cluster %q has no KRMSyncer %s", c.name, syncerNameOf(&sts))
				continue
			}
			if got := syncer.Spec.Remote.GKECluster.Name; got != leader.gke.Name {
				t.Errorf("cluster %q: KRMSyncer %s names cluster %q as its remote, want the leader %q", c.name, syncer.Name, got, leader.gke.Name)
			}
			if got, want := syncer.Spec.Suspend, c == leader; got != want {
				t.Errorf("cluster %q: KRMSyncer %s has suspend=%v, want %v", c.name, syncer.Name, got, want)
			}
		}
	}
}

// requirePatches checks failover's writes, and what the fleet looked like at each one.
func (f *haFleet) requirePatches(want ...string) {
	f.t.Helper()
	got := make([]string, 0, len(f.patches))
	for _, p := range f.patches {
		got = append(got, p.String())
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		f.t.Errorf("failover's writes:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}
