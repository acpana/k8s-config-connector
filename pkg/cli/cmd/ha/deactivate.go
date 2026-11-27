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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	gcs "cloud.google.com/go/storage"
	operatorv1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/operator/pkg/apis/core/v1beta1"
	operatork8s "github.com/GoogleCloudPlatform/k8s-config-connector/operator/pkg/k8s"
	syncerv1alpha1 "github.com/gke-labs/kube-etl/syncer/api/v1alpha1"
	mclv1alpha1 "github.com/gke-labs/multicluster-leader-election/api/v1alpha1"
	mclstorage "github.com/gke-labs/multicluster-leader-election/pkg/storage"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	standaloneClusterFlag = "standalone-cluster"

	defaultDeactivateTimeout           = 5 * time.Minute
	minDeactivateTimeout               = time.Minute
	defaultDeactivatePollInterval      = 2 * time.Second
	defaultDeactivateClusterOpTimeout  = time.Minute
	defaultDeactivateLeaseCloseTimeout = 30 * time.Second
	defaultDeactivatePodDeleteDelay    = 30 * time.Second
	maxConfigConnectorUpdateRetries    = 10
)

const deactivateLong = `Turn multi-cluster HA off for a group so one standalone cluster reconciles on its own.

Deactivate closes the multi-cluster lease in GCS first so no cluster can win leader election,
removes spec.experiments.multiClusterLease from the standalone cluster's ConfigConnector (saving
the removed block in the ` + deactivatedLeaseAnnotation + ` annotation), parks the other
clusters in --contexts (scaling their manager StatefulSets and MultiClusterLease controller to 0
with ` + deactivatedReplicasAnnotation + ` and suspending their KRMSyncers), and waits until
the standalone cluster's managers restart and reconcile without leader election.

The standalone cluster must already be the leader of the HA group: deactivate never moves
leadership. If another cluster holds the lease, run "config-connector ha failover" first or pass
that cluster as --standalone-cluster.

The other clusters keep their copies of the resources and their spec.experiments.multiClusterLease
configuration, and the HA components (CRDs, RBAC, Deployments, IAM bindings) and lease bucket
remain installed. With both left in place, a manager that starts on another cluster cannot lead,
and "config-connector ha activate" can turn HA on again later.

Running this command again is safe: if it is interrupted or fails partway through, re-running it
continues from where it stopped.`

// leaseStoreFactory creates a storage.Storage backed by the named GCS bucket (or a fake in tests)
// and an optional cleanup callback.
type leaseStoreFactory func(ctx context.Context, bucket string) (mclstorage.Storage, func() error, error)

// deactivateOptions holds flags and configuration for `config-connector ha deactivate`.
type deactivateOptions struct {
	baseOptions

	standaloneCluster string
	timeout           time.Duration
	dryRun            bool
	bucket            string

	runner             commandRunner
	newLeaseStore      leaseStoreFactory
	pollInterval       time.Duration
	clusterOpTimeout   time.Duration
	leaseCloseTimeout  time.Duration
	leaseRetryInterval time.Duration
	podDeleteDelay     time.Duration
	signalBounceWindow time.Duration
	nowFunc            func() time.Time

	// onAfterStep is an optional test hook called after each step completes to simulate abrupt
	// termination (such as SIGKILL) between steps.
	onAfterStep func(step string) error
}

func newDeactivateOptions() deactivateOptions {
	return deactivateOptions{
		timeout:            defaultDeactivateTimeout,
		pollInterval:       defaultDeactivatePollInterval,
		clusterOpTimeout:   defaultDeactivateClusterOpTimeout,
		leaseCloseTimeout:  defaultDeactivateLeaseCloseTimeout,
		leaseRetryInterval: 50 * time.Millisecond,
		podDeleteDelay:     defaultDeactivatePodDeleteDelay,
		signalBounceWindow: defaultSignalBounceWindow,
	}
}

func NewDeactivateCmd() *cobra.Command {
	return newDeactivateCmd(newDeactivateOptions())
}

func newDeactivateCmd(opts deactivateOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deactivate",
		Short: "Turn multi-cluster HA off so one standalone cluster reconciles on its own",
		Long:  deactivateLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.stdout = cmd.OutOrStdout()
			opts.stderr = cmd.ErrOrStderr()
			if err := opts.validate(); err != nil {
				return err
			}
			// From here on, errors are about the clusters or lease, not about CLI usage.
			cmd.SilenceUsage = true

			signal.Ignore(syscall.SIGPIPE)
			ctx, stop := notifyInterruptSignals(
				cmd.Context(),
				opts.stderr,
				opts.signalBounceWindow,
				"\nInterrupted; stopping cleanly — run this command again to finish (press Ctrl-C again to exit at once).",
			)
			defer stop()
			return runDeactivate(ctx, opts)
		},
	}
	opts.addFlags(cmd.Flags())
	return cmd
}

func (o *deactivateOptions) addFlags(flags *pflag.FlagSet) {
	flags.StringSliceVar(&o.contexts, contextsFlag, o.contexts, "Comma-separated list of kubeconfig contexts to act on (must include --standalone-cluster; may name that cluster alone).")
	flags.StringVar(&o.standaloneCluster, standaloneClusterFlag, o.standaloneCluster, "Kubeconfig context name of the leader cluster that will keep reconciling without leader election.")
	flags.DurationVar(&o.timeout, failoverTimeoutFlag, o.timeout, "How long to wait for managers to stop or to restart (at least 1m).")
	flags.BoolVar(&o.dryRun, dryRunFlag, o.dryRun, "If true, run the preflight checks and print what steps 1 to 4 would do without making changes.")
	flags.StringVar(&o.bucket, bucketFlag, o.bucket, "GCS bucket holding the multi-cluster lease. Defaults to the --gcs-bucket argument of Deployment cnrm-system/multiclusterlease-controller-manager on the standalone cluster.")
}

func (o *deactivateOptions) validate() error {
	var errs []error
	if err := o.baseOptions.validateMinContexts(1); err != nil {
		errs = append(errs, err)
	}
	o.standaloneCluster = strings.TrimSpace(o.standaloneCluster)
	if o.standaloneCluster == "" {
		errs = append(errs, fmt.Errorf("--%s is required", standaloneClusterFlag))
	} else if !slices.Contains(o.contexts, o.standaloneCluster) {
		errs = append(errs, fmt.Errorf("--%s %q must be one of the contexts specified in --%s (%v)", standaloneClusterFlag, o.standaloneCluster, contextsFlag, o.contexts))
	}
	if o.timeout < minDeactivateTimeout {
		errs = append(errs, fmt.Errorf("--%s must be at least %s, got %s", failoverTimeoutFlag, minDeactivateTimeout, o.timeout))
	}
	o.bucket = normalizeBucketName(o.bucket)
	if o.bucket != "" {
		if err := validateBucketName(o.bucket); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func runDeactivate(ctx context.Context, opts deactivateOptions) error {
	handles, err := buildClusterHandles(ctx, opts.contexts)
	if err != nil {
		return err
	}
	return executeDeactivate(ctx, opts, handles)
}

func executeDeactivate(ctx context.Context, opts deactivateOptions, handles []*clusterHandle) error {
	d, err := newDeactivator(opts, handles)
	if err != nil {
		return err
	}
	return d.run(ctx)
}

// newGCSLeaseStore builds a GCS-backed storage.Storage authenticated with an access token from
// `gcloud auth print-access-token` read via runner.output.
func newGCSLeaseStore(ctx context.Context, runner commandRunner, bucket string) (mclstorage.Storage, func() error, error) {
	rawToken, err := runner.output(ctx, "gcloud", "auth", "print-access-token")
	if err != nil {
		return nil, nil, fmt.Errorf("obtaining GCP access token via gcloud: %w", err)
	}
	token := strings.TrimSpace(string(rawToken))
	if token == "" {
		return nil, nil, fmt.Errorf("obtaining GCP access token via gcloud: empty token returned")
	}
	client, err := gcs.NewClient(ctx, option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})))
	if err != nil {
		return nil, nil, fmt.Errorf("creating GCS client for bucket %q: %w", bucket, err)
	}
	return mclstorage.NewGCSStorage(client, bucket), client.Close, nil
}

// deactivateCluster holds the Step 0 snapshot of one cluster in --contexts.
type deactivateCluster struct {
	handle *clusterHandle

	cc            *operatorv1beta1.ConfigConnector
	leaseSpec     *operatorv1beta1.MultiClusterLeaseSpec
	identity      string
	isStandalone  bool
	lease         *mclv1alpha1.MultiClusterLease
	managers      []*managerStatefulSet
	mclDeployment *appsv1.Deployment
	mclBucket     string
	syncers       []syncerv1alpha1.KRMSyncer
}

func (c *deactivateCluster) name() string {
	return c.handle.contextName
}

// deactivatePlan records what Step 0 decided must happen in Steps 1–4.
type deactivatePlan struct {
	bucket   string
	leaseKey string
	// leaseObjectURI is "gs://<bucket>/leases/<namespace>/<name>".
	leaseObjectURI string

	// needCloseLease is true if the GCS lease object must be written to close it for X (or to
	// rewrite a bare "config-connector-ha-deactivated" marker to include X's identity).
	needCloseLease bool
	// rewriteBareClosed is true when the lease is already closed without an identity and must be
	// rewritten to include X's identity.
	rewriteBareClosed bool
	// needUpdateX is true if X's ConfigConnector still has spec.experiments.multiClusterLease.
	needUpdateX bool
	// initialHolderWasX is true if the lease named X in Step 0 (used for header formatting).
	initialHolderWasX bool
}

type deactivator struct {
	handles           []*clusterHandle
	standaloneContext string

	out                io.Writer
	dryRun             bool
	explicitBucket     string
	newLeaseStore      leaseStoreFactory
	timeout            time.Duration
	pollInterval       time.Duration
	clusterOpTimeout   time.Duration
	leaseCloseTimeout  time.Duration
	leaseRetryInterval time.Duration
	podDeleteDelay     time.Duration
	nowFunc            func() time.Time
	onAfterStep        func(step string) error

	clusters   []*deactivateCluster
	standalone *deactivateCluster
	leaseName  string
	leaseNS    string
}

func newDeactivator(opts deactivateOptions, handles []*clusterHandle) (*deactivator, error) {
	out := opts.stdout
	if out == nil {
		out = io.Discard
	}
	runner := opts.runner
	if runner == nil {
		runner = newExecCommandRunner(out, opts.stderr, opts.dryRun)
	}
	storeFactory := opts.newLeaseStore
	if storeFactory == nil {
		storeFactory = func(ctx context.Context, bucket string) (mclstorage.Storage, func() error, error) {
			return newGCSLeaseStore(ctx, runner, bucket)
		}
	}
	opTimeout := opts.clusterOpTimeout
	if opTimeout <= 0 {
		opTimeout = defaultDeactivateClusterOpTimeout
	}
	pollInterval := opts.pollInterval
	if pollInterval <= 0 {
		pollInterval = defaultDeactivatePollInterval
	}
	leaseCloseTimeout := opts.leaseCloseTimeout
	if leaseCloseTimeout <= 0 {
		leaseCloseTimeout = defaultDeactivateLeaseCloseTimeout
	}
	podDeleteDelay := opts.podDeleteDelay
	if podDeleteDelay <= 0 {
		podDeleteDelay = defaultDeactivatePodDeleteDelay
	}
	nowFn := opts.nowFunc
	if nowFn == nil {
		nowFn = time.Now
	}

	return &deactivator{
		handles:            handles,
		standaloneContext:  opts.standaloneCluster,
		out:                out,
		dryRun:             opts.dryRun,
		explicitBucket:     opts.bucket,
		newLeaseStore:      storeFactory,
		timeout:            opts.timeout,
		pollInterval:       pollInterval,
		clusterOpTimeout:   opTimeout,
		leaseCloseTimeout:  leaseCloseTimeout,
		leaseRetryInterval: opts.leaseRetryInterval,
		podDeleteDelay:     podDeleteDelay,
		nowFunc:            nowFn,
		onAfterStep:        opts.onAfterStep,
	}, nil
}

func (d *deactivator) logf(format string, args ...any) {
	fmt.Fprintf(d.out, format+"\n", args...)
}

func (d *deactivator) run(ctx context.Context) error {
	// Step 0: Checks. Nothing is changed.
	plan, store, closeStore, err := d.check(ctx)
	if err != nil {
		return err
	}
	if closeStore != nil {
		defer closeStore()
	}

	if plan.initialHolderWasX {
		d.logf("Leader: %s", d.standalone.name())
	} else {
		d.logf("Standalone: %s", d.standalone.name())
	}
	d.logf("Lease: %s", plan.leaseObjectURI)

	if d.dryRun {
		d.printDryRunPlan(plan)
		return nil
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("interrupted: %w", err)
	}

	if d.hasWork(plan) {
		d.logf("If this command doesn't finish, run it again: it continues where it stopped.")
	}

	// Steps 1 and 2 run without the command's cancellation so Ctrl-C between them cannot leave the
	// lease closed with the standalone cluster still configured for leader election.
	var (
		didCloseLease    bool
		didStep2         bool
		priorLease       mclstorage.LeaseData
		closedLease      *mclstorage.LeaseObject
		podSnapshot      map[string]map[string]*managerPodRecord
		waitRestartSince time.Time
	)

	uncancelledCtx := context.WithoutCancel(ctx)

	// If X is already standalone and the lease still needs closing, snapshot X's manager pods
	// before Step 1 so restarts triggered by closing the lease are recognized in Step 4.
	if plan.needCloseLease && !plan.needUpdateX {
		snapCtx, cancel := context.WithTimeout(uncancelledCtx, d.clusterOpTimeout)
		podSnapshot, err = d.recordStandaloneManagerPods(snapCtx)
		cancel()
		if err != nil {
			return err
		}
	}

	// Step 1: Close the lease.
	if plan.needCloseLease {
		var closedByUs bool
		priorLease, closedLease, closedByUs, err = d.closeLease(uncancelledCtx, store, plan)
		if err != nil {
			return err
		}
		didCloseLease = closedByUs
		if !plan.needUpdateX {
			waitRestartSince = d.nowFunc()
		}
		if d.onAfterStep != nil {
			if err := d.onAfterStep("step1"); err != nil {
				return err
			}
		}
	}

	// Step 2: Make X standalone.
	if plan.needUpdateX {
		step2Err := d.makeStandalone(uncancelledCtx)
		if step2Err != nil {
			if didCloseLease {
				rollbackCtx, cancel := context.WithTimeout(uncancelledCtx, d.leaseCloseTimeout)
				rollbackErr := d.restoreLease(rollbackCtx, store, plan, priorLease, closedLease.ResourceVersion)
				cancel()
				if rollbackErr != nil {
					return fmt.Errorf(
						"updating ConfigConnector on cluster %q failed (%v), and restoring the lease object %s also failed (%v): the lease is closed and cluster %q still has leader election configured: no cluster reconciles. Run the command again.",
						d.standalone.name(), step2Err, plan.leaseObjectURI, rollbackErr, d.standalone.name(),
					)
				}
				return fmt.Errorf("making cluster %q standalone failed (the lease was restored to its previous state): %w", d.standalone.name(), step2Err)
			}
			return fmt.Errorf(
				"updating ConfigConnector on cluster %q failed (%v): the lease is closed and cluster %q still has leader election configured: no cluster reconciles. Run the command again.",
				d.standalone.name(), step2Err, d.standalone.name(),
			)
		}
		didStep2 = true
		d.logf("cluster %q: removed spec.experiments.multiClusterLease from its ConfigConnector", d.standalone.name())

		// Record every manager pod of X right after Step 2, before Step 3 takes time.
		snapCtx, cancel := context.WithTimeout(uncancelledCtx, d.clusterOpTimeout)
		podSnapshot, err = d.recordStandaloneManagerPods(snapCtx)
		cancel()
		if err != nil {
			return err
		}
		waitRestartSince = d.nowFunc()

		if d.onAfterStep != nil {
			if err := d.onAfterStep("step2"); err != nil {
				return err
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("interrupted after updating %q; run the command again to finish: %w", d.standalone.name(), err)
	}

	// Step 3: Park the other clusters and ensure X's own KRMSyncers stay suspended.
	parkErrs, interrupted := d.parkOtherClusters(ctx)
	if interrupted != nil {
		return interrupted
	}
	if err := d.ensureStandaloneSyncersSuspended(ctx); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted: %w", ctx.Err())
		}
		parkErrs = append(parkErrs, err)
	}
	if d.onAfterStep != nil {
		if err := d.onAfterStep("step3"); err != nil {
			return err
		}
	}

	// Step 4: Wait until X reconciles on its own.
	needWaitRestart := didStep2 || (didCloseLease && !plan.rewriteBareClosed)
	if err := d.waitForStandalone(ctx, needWaitRestart, podSnapshot, waitRestartSince); err != nil {
		if len(parkErrs) > 0 {
			return errors.Join(append(parkErrs, err)...)
		}
		return err
	}

	if len(parkErrs) > 0 {
		return errors.Join(parkErrs...)
	}

	// Step 5: Report.
	d.printReport()
	return nil
}

// check implements Step 0: read all clusters, validate configuration and bucket, read the GCS lease,
// apply the decision table, check failover/manager invariants, and perform server-side dry-runs.
func (d *deactivator) check(ctx context.Context) (*deactivatePlan, mclstorage.Storage, func() error, error) {
	if len(d.handles) == 0 {
		return nil, nil, nil, fmt.Errorf("at least 1 cluster context must be specified via --%s", contextsFlag)
	}

	// 1. Fetch ConfigConnector on every cluster first.
	d.clusters = make([]*deactivateCluster, 0, len(d.handles))
	var fetchErrs []error
	for _, h := range d.handles {
		callCtx, cancel := context.WithTimeout(ctx, d.clusterOpTimeout)
		cc, err := fetchConfigConnector(callCtx, h)
		cancel()
		if err != nil {
			fetchErrs = append(fetchErrs, err)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		c := &deactivateCluster{handle: h, cc: cc}
		d.clusters = append(d.clusters, c)
		if h.contextName == d.standaloneContext {
			d.standalone = c
		}
	}
	if len(fetchErrs) > 0 {
		return nil, nil, nil, errors.Join(fetchErrs...)
	}
	if d.standalone == nil {
		return nil, nil, nil, fmt.Errorf("--%s %q is not one of the contexts in --%s", standaloneClusterFlag, d.standaloneContext, contextsFlag)
	}

	// 2. Inspect ConfigConnector on every cluster for active or deactivated lease configuration.
	var leaseSpecs []namedLeaseSpec
	var otherStandalone []*deactivateCluster
	for _, c := range d.clusters {
		mcl := multiClusterLeaseSpec(c.cc)
		if mcl != nil {
			if err := validateClusterLeaseSpec(c.name(), mcl); err != nil {
				return nil, nil, nil, err
			}
			c.leaseSpec = mcl
			c.identity, _ = mcl.CandidateIdentity()
			leaseSpecs = append(leaseSpecs, namedLeaseSpec{contextName: c.name(), spec: mcl})
			continue
		}
		deactSpec, hasDeact, err := deactivatedLeaseSpec(c.name(), c.cc)
		if err != nil {
			return nil, nil, nil, err
		}
		if hasDeact {
			c.leaseSpec = deactSpec
			c.identity, _ = deactSpec.CandidateIdentity()
			c.isStandalone = true
			if c != d.standalone {
				otherStandalone = append(otherStandalone, c)
			}
			leaseSpecs = append(leaseSpecs, namedLeaseSpec{contextName: c.name(), spec: deactSpec})
			continue
		}
		return nil, nil, nil, fmt.Errorf(
			"cluster %q has neither spec.experiments.multiClusterLease nor the %s annotation on ConfigConnector %q; run \"config-connector ha status\" to inspect the group",
			c.name(), deactivatedLeaseAnnotation, configConnectorResourceName,
		)
	}

	if err := validateMultiClusterLeaseSpecs(leaseSpecs); err != nil {
		return nil, nil, nil, err
	}
	d.leaseName = d.standalone.leaseSpec.LeaseName
	d.leaseNS = d.standalone.leaseSpec.Namespace

	// Row 5 (other cluster standalone).
	if len(otherStandalone) > 0 {
		return nil, nil, nil, fmt.Errorf("multi-cluster HA is already deactivated, with cluster %q reconciling on its own", otherStandalone[0].name())
	}

	// 3. Read MultiClusterLease, manager StatefulSets, MCL Deployment, and KRMSyncers on every cluster.
	for _, c := range d.clusters {
		callCtx, cancel := context.WithTimeout(ctx, d.clusterOpTimeout)
		err := d.fetchClusterResources(callCtx, c)
		cancel()
		if err != nil {
			return nil, nil, nil, err
		}
	}

	// 4. Find the bucket and confirm every named cluster's MCL controller agrees on it.
	bucket, err := d.resolveBucket()
	if err != nil {
		return nil, nil, nil, err
	}

	// 5. Open the lease store and read the lease object.
	leaseKey := d.leaseNS + "/" + d.leaseName
	leaseObjectURI := fmt.Sprintf("gs://%s/leases/%s", bucket, leaseKey)

	storeCtx, cancel := context.WithTimeout(ctx, d.clusterOpTimeout)
	store, closeStore, err := d.newLeaseStore(storeCtx, bucket)
	cancel()
	if err != nil {
		return nil, nil, nil, err
	}
	cleanupOnErr := func() {
		if closeStore != nil {
			_ = closeStore()
		}
	}

	readCtx, cancel := context.WithTimeout(ctx, d.clusterOpTimeout)
	leaseObj, err := store.ReadLease(readCtx, leaseKey)
	cancel()
	if err != nil {
		cleanupOnErr()
		if store.IsNotFound(err) {
			return nil, nil, nil, fmt.Errorf("lease object %s does not exist; run \"config-connector ha status\" to inspect the group", leaseObjectURI)
		}
		if isLeasePermissionError(err) {
			return nil, nil, nil, leasePermissionError("reading", bucket, leaseObjectURI, err)
		}
		return nil, nil, nil, fmt.Errorf("reading lease object %s: %w", leaseObjectURI, err)
	}

	plan, err := d.decidePlan(bucket, leaseKey, leaseObjectURI, leaseObj)
	if err != nil {
		cleanupOnErr()
		return nil, nil, nil, err
	}

	// 6. Refuse if an interrupted ha failover left managers stopped on any named cluster.
	for _, c := range d.clusters {
		if len(stoppedByFailover(c.managers)) > 0 {
			cleanupOnErr()
			return nil, nil, nil, fmt.Errorf("an interrupted \"ha failover\" left managers stopped on cluster %q; run \"config-connector ha failover\" again first", c.name())
		}
	}

	// 7. Refuse if X's managers are scaled to 0 (or missing in cluster mode).
	if len(d.standalone.managers) == 0 && d.standalone.cc.GetMode() != operatork8s.NamespacedMode {
		cleanupOnErr()
		return nil, nil, nil, fmt.Errorf("cluster %q has no StatefulSet of Config Connector's manager", d.standalone.name())
	}
	for _, m := range d.standalone.managers {
		if m.replicas() == 0 {
			cleanupOnErr()
			return nil, nil, nil, fmt.Errorf("cluster %q: manager StatefulSet %s is scaled to 0; scale it up before deactivating HA", d.standalone.name(), m.key())
		}
	}

	// 8. Server-side dry-run of the ConfigConnector update on X and patches on the clusters.
	if err := d.serverSideDryRun(ctx, plan); err != nil {
		cleanupOnErr()
		return nil, nil, nil, err
	}

	return plan, store, closeStore, nil
}

func (d *deactivator) fetchClusterResources(ctx context.Context, c *deactivateCluster) error {
	lease, err := fetchMultiClusterLease(ctx, c.handle, d.leaseNS, d.leaseName)
	if err != nil {
		return err
	}
	c.lease = lease

	managers, err := c.handle.listManagers(ctx)
	if err != nil {
		return err
	}
	c.managers = managers

	mclDep := &appsv1.Deployment{}
	err = c.handle.client.Get(ctx, types.NamespacedName{Namespace: manifestSystemNamespace, Name: mclDeploymentName}, mclDep)
	switch {
	case err == nil:
		c.mclDeployment = mclDep
		c.mclBucket = extractGCSBucketArg(mclDep)
	case apierrors.IsNotFound(err):
		c.mclDeployment = nil
	default:
		return fmt.Errorf("fetching Deployment %s/%s on cluster %q: %w", manifestSystemNamespace, mclDeploymentName, c.name(), err)
	}

	syncers, err := listKRMSyncers(ctx, c.handle, d.leaseNS)
	if err != nil {
		return err
	}
	c.syncers = leaseSyncers(syncers, d.leaseName)
	return nil
}

func (d *deactivator) resolveBucket() (string, error) {
	bucket := d.explicitBucket
	if bucket == "" {
		bucket = d.standalone.mclBucket
		if bucket == "" {
			return "", fmt.Errorf(
				"could not determine the lease bucket from Deployment %s/%s on cluster %q; pass --%s explicitly",
				manifestSystemNamespace, mclDeploymentName, d.standalone.name(), bucketFlag,
			)
		}
	}
	for _, c := range d.clusters {
		if c.mclBucket != "" && c.mclBucket != bucket {
			return "", fmt.Errorf(
				"cluster %q's Deployment %s/%s uses lease bucket %q, which differs from %q",
				c.name(), manifestSystemNamespace, mclDeploymentName, c.mclBucket, bucket,
			)
		}
	}
	return bucket, nil
}

func (d *deactivator) clusterByIdentity(identity string) *deactivateCluster {
	for _, c := range d.clusters {
		if c.identity == identity {
			return c
		}
	}
	return nil
}

func (d *deactivator) decidePlan(bucket, leaseKey, leaseObjectURI string, leaseObj *mclstorage.LeaseObject) (*deactivatePlan, error) {
	plan := &deactivatePlan{
		bucket:         bucket,
		leaseKey:       leaseKey,
		leaseObjectURI: leaseObjectURI,
	}

	holder := leaseObj.Data.HolderIdentity
	closedID, isClosed := parseClosedLeaseHolder(holder)

	if isClosed {
		switch {
		case closedID != "" && closedID != d.standalone.identity:
			if other := d.clusterByIdentity(closedID); other != nil {
				return nil, fmt.Errorf("multi-cluster HA is already deactivated, with cluster %q reconciling on its own", other.name())
			}
			return nil, fmt.Errorf("multi-cluster HA is already deactivated, with cluster %q (not in --%s) reconciling on its own", closedID, contextsFlag)

		case closedID == "":
			if !d.standalone.isStandalone {
				return nil, fmt.Errorf(
					"lease %s is closed without a cluster identity (%q), so this command cannot tell for which cluster it was closed; include the cluster that reconciles on its own in --%s",
					leaseObjectURI, holder, contextsFlag,
				)
			}
			// Row 3: X is standalone already; rewrite bare marker to include X's identity.
			plan.needCloseLease = true
			plan.rewriteBareClosed = true
			plan.needUpdateX = false
			return plan, nil

		default: // closedID == d.standalone.identity
			if !d.standalone.isStandalone {
				// Row 2: A run died between steps 1 and 2. Continue with step 2.
				plan.needCloseLease = !leaseObj.Data.RenewTime.Equal(deactivatedLeaseRenewTime)
				plan.needUpdateX = true
				return plan, nil
			}
			// Row 3: X is standalone already and the lease is already closed for X.
			plan.needCloseLease = !leaseObj.Data.RenewTime.Equal(deactivatedLeaseRenewTime)
			plan.needUpdateX = false
			return plan, nil
		}
	}

	if holder == "" {
		return nil, fmt.Errorf("lease object %s has no holder; run \"config-connector ha status\" to inspect the group", leaseObjectURI)
	}

	if holder != d.standalone.identity {
		// Row 4: Another cluster Z holds the lease.
		if leader := d.clusterByIdentity(holder); leader != nil {
			return nil, fmt.Errorf(
				"cluster %q is the leader. Run \"config-connector ha failover --%s=%s\" first, or pass --%s=%s",
				leader.name(), targetClusterFlag, d.standalone.name(), standaloneClusterFlag, leader.name(),
			)
		}
		return nil, fmt.Errorf(
			"cluster %q is the leader, and it is not in --%s. Run \"config-connector ha failover --%s=%s\" first, or include it and pass it to --%s",
			holder, contextsFlag, targetClusterFlag, d.standalone.name(), standaloneClusterFlag,
		)
	}

	// holder == d.standalone.identity
	plan.initialHolderWasX = true
	plan.needCloseLease = true
	plan.needUpdateX = !d.standalone.isStandalone
	return plan, nil
}

func deactivateConfigConnector(cc *operatorv1beta1.ConfigConnector) error {
	mcl := multiClusterLeaseSpec(cc)
	if mcl == nil {
		return fmt.Errorf("ConfigConnector %q has no spec.experiments.multiClusterLease", cc.Name)
	}
	raw, err := json.Marshal(mcl)
	if err != nil {
		return fmt.Errorf("marshalling spec.experiments.multiClusterLease: %w", err)
	}
	if cc.Annotations == nil {
		cc.Annotations = map[string]string{}
	}
	cc.Annotations[deactivatedLeaseAnnotation] = string(raw)
	cc.Spec.Experiments.MultiClusterLease = nil
	if cc.Spec.Experiments.ResourceSettings == nil && len(cc.Spec.Experiments.MaxConcurrentReconcilesOverrides) == 0 {
		cc.Spec.Experiments = nil
	}
	return nil
}

var suspendSyncerPatch = client.RawPatch(types.MergePatchType, []byte(`{"spec":{"suspend":true}}`))

func (d *deactivator) serverSideDryRun(ctx context.Context, plan *deactivatePlan) error {
	if plan.needUpdateX {
		ccCopy := d.standalone.cc.DeepCopy()
		if err := deactivateConfigConnector(ccCopy); err != nil {
			return err
		}
		callCtx, cancel := context.WithTimeout(ctx, d.clusterOpTimeout)
		err := d.standalone.handle.client.Update(callCtx, ccCopy, client.DryRunAll)
		cancel()
		if err != nil {
			return fmt.Errorf("dry-run update of ConfigConnector %q on cluster %q failed (nothing was changed): %w", ccCopy.Name, d.standalone.name(), err)
		}
	}

	for i := range d.standalone.syncers {
		s := &d.standalone.syncers[i]
		if s.Spec.Suspend {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, d.clusterOpTimeout)
		err := d.standalone.handle.client.Patch(callCtx, s.DeepCopy(), suspendSyncerPatch, client.DryRunAll)
		cancel()
		if err != nil {
			return fmt.Errorf("dry-run patch of KRMSyncer %s/%s on cluster %q failed (nothing was changed): %w", s.Namespace, s.Name, d.standalone.name(), err)
		}
	}

	for _, c := range d.clusters {
		if c == d.standalone {
			continue
		}
		for _, m := range c.managers {
			replicas := m.replicas()
			if replicas == 0 {
				continue
			}
			ann := strconv.Itoa(int(replicas))
			patch, err := managerPatch(deactivatedReplicasAnnotation, 0, &ann)
			if err != nil {
				return err
			}
			callCtx, cancel := context.WithTimeout(ctx, d.clusterOpTimeout)
			err = c.handle.client.Patch(callCtx, m.StatefulSet.DeepCopy(), patch, client.DryRunAll)
			cancel()
			if err != nil {
				return fmt.Errorf("dry-run patch of StatefulSet %s on cluster %q failed (nothing was changed): %w", m.key(), c.name(), err)
			}
		}
		for i := range c.syncers {
			s := &c.syncers[i]
			if s.Spec.Suspend {
				continue
			}
			callCtx, cancel := context.WithTimeout(ctx, d.clusterOpTimeout)
			err := c.handle.client.Patch(callCtx, s.DeepCopy(), suspendSyncerPatch, client.DryRunAll)
			cancel()
			if err != nil {
				return fmt.Errorf("dry-run patch of KRMSyncer %s/%s on cluster %q failed (nothing was changed): %w", s.Namespace, s.Name, c.name(), err)
			}
		}
		if c.mclDeployment != nil {
			if replicas := deploymentReplicas(c.mclDeployment); replicas > 0 {
				ann := strconv.Itoa(int(replicas))
				patch, err := managerPatch(deactivatedReplicasAnnotation, 0, &ann)
				if err != nil {
					return err
				}
				callCtx, cancel := context.WithTimeout(ctx, d.clusterOpTimeout)
				err = c.handle.client.Patch(callCtx, c.mclDeployment.DeepCopy(), patch, client.DryRunAll)
				cancel()
				if err != nil {
					return fmt.Errorf("dry-run patch of Deployment %s/%s on cluster %q failed (nothing was changed): %w", c.mclDeployment.Namespace, c.mclDeployment.Name, c.name(), err)
				}
			}
		}
	}

	return nil
}

func (d *deactivator) hasWork(plan *deactivatePlan) bool {
	if plan.needCloseLease || plan.needUpdateX {
		return true
	}
	for _, s := range d.standalone.syncers {
		if !s.Spec.Suspend {
			return true
		}
	}
	for _, c := range d.clusters {
		if c == d.standalone {
			continue
		}
		for _, m := range c.managers {
			if m.replicas() > 0 || m.pendingStopped() != "" {
				return true
			}
		}
		for _, s := range c.syncers {
			if !s.Spec.Suspend {
				return true
			}
		}
		if c.mclDeployment != nil && (deploymentReplicas(c.mclDeployment) > 0 || deploymentPendingStopped(c.mclDeployment) != "") {
			return true
		}
	}
	return false
}

func (d *deactivator) printDryRunPlan(plan *deactivatePlan) {
	if !d.hasWork(plan) {
		d.logf("[dry-run] multi-cluster HA is already deactivated: %s reconciles on its own; nothing would be changed", d.standalone.name())
		return
	}
	if plan.needCloseLease {
		if plan.rewriteBareClosed {
			d.logf("[dry-run] lease: would rewrite closed holder to %s", closedLeaseHolder(d.standalone.identity))
		} else {
			d.logf("[dry-run] lease: would close %s for cluster %q so no cluster can take it", plan.leaseObjectURI, d.standalone.name())
		}
	}
	if plan.needUpdateX {
		d.logf("[dry-run] cluster %q: would remove spec.experiments.multiClusterLease from its ConfigConnector and save it in annotation %s", d.standalone.name(), deactivatedLeaseAnnotation)
	}
	for _, c := range d.clusters {
		if c == d.standalone {
			continue
		}
		var running []*managerStatefulSet
		for _, m := range c.managers {
			if m.replicas() > 0 {
				running = append(running, m)
			} else if !hasDeactivatedReplicas(m.StatefulSet) {
				d.logf("[dry-run] cluster %q: StatefulSet %s is already scaled to 0 without %s; would leave it alone (ha activate will not start it)", c.name(), m.key(), deactivatedReplicasAnnotation)
			}
		}
		if len(running) > 0 {
			d.logf("[dry-run] cluster %q: would stop %s, and wait up to %s for its manager pods to stop", c.name(), describeManagers(running), d.timeout)
		}
		unsuspended := countUnsuspendedSyncers(c.syncers)
		if unsuspended > 0 {
			d.logf("[dry-run] cluster %q: would suspend %d %s", c.name(), unsuspended, pluralSyncer(unsuspended))
		}
		if c.mclDeployment != nil && deploymentReplicas(c.mclDeployment) > 0 {
			d.logf("[dry-run] cluster %q: would stop Deployment %s/%s, and wait up to %s for its pod to stop", c.name(), c.mclDeployment.Namespace, c.mclDeployment.Name, d.timeout)
		}
	}
	if unsuspended := countUnsuspendedSyncers(d.standalone.syncers); unsuspended > 0 {
		d.logf("[dry-run] cluster %q: would suspend %d %s", d.standalone.name(), unsuspended, pluralSyncer(unsuspended))
	}
	if plan.needUpdateX || (plan.needCloseLease && !plan.rewriteBareClosed) {
		d.logf("[dry-run] cluster %q: would wait up to %s for its managers to restart without leader election", d.standalone.name(), d.timeout)
	}
}

// closeLease implements Step 1: read-check-write the GCS lease object with optimistic locking,
// retrying on conflict for up to leaseCloseTimeout.
func (d *deactivator) closeLease(ctx context.Context, store mclstorage.Storage, plan *deactivatePlan) (mclstorage.LeaseData, *mclstorage.LeaseObject, bool, error) {
	deadline := d.nowFunc().Add(d.leaseCloseTimeout)
	wantHolder := closedLeaseHolder(d.standalone.identity)

	for {
		remaining := deadline.Sub(d.nowFunc())
		if remaining <= 0 {
			return mclstorage.LeaseData{}, nil, false, fmt.Errorf(
				"timed out after %s trying to close lease %s due to concurrent updates; nothing was changed",
				d.leaseCloseTimeout, plan.leaseObjectURI,
			)
		}

		callCtx, cancel := context.WithTimeout(ctx, remaining)
		obj, err := store.ReadLease(callCtx, plan.leaseKey)
		cancel()
		if err != nil {
			if store.IsNotFound(err) {
				return mclstorage.LeaseData{}, nil, false, fmt.Errorf("lease object %s no longer exists; nothing was changed", plan.leaseObjectURI)
			}
			if isLeasePermissionError(err) {
				return mclstorage.LeaseData{}, nil, false, leasePermissionError("reading", plan.bucket, plan.leaseObjectURI, err)
			}
			return mclstorage.LeaseData{}, nil, false, fmt.Errorf("reading lease object %s: %w", plan.leaseObjectURI, err)
		}

		holder := obj.Data.HolderIdentity
		if holder == wantHolder && obj.Data.RenewTime.Equal(deactivatedLeaseRenewTime) {
			d.logf("lease: closed, no cluster can take it")
			return obj.Data, obj, false, nil
		}

		allowed := holder == d.standalone.identity
		if !allowed && d.standalone.isStandalone {
			closedID, isClosed := parseClosedLeaseHolder(holder)
			allowed = isClosed && (closedID == "" || closedID == d.standalone.identity)
		}
		if !allowed && !d.standalone.isStandalone {
			// Also allow completing renewTime when already closed for X.
			closedID, isClosed := parseClosedLeaseHolder(holder)
			allowed = isClosed && closedID == d.standalone.identity
		}
		if !allowed {
			return mclstorage.LeaseData{}, nil, false, fmt.Errorf(
				"leadership moved to %q while closing lease %s; nothing was changed",
				holder, plan.leaseObjectURI,
			)
		}

		prior := obj.Data
		toWrite := &mclstorage.LeaseObject{
			ResourceVersion: obj.ResourceVersion,
			Data: mclstorage.LeaseData{
				HolderIdentity:   wantHolder,
				RenewTime:        deactivatedLeaseRenewTime,
				LeaseTransitions: obj.Data.LeaseTransitions,
			},
		}

		remaining = deadline.Sub(d.nowFunc())
		if remaining <= 0 {
			return mclstorage.LeaseData{}, nil, false, fmt.Errorf(
				"timed out after %s trying to close lease %s due to concurrent updates; nothing was changed",
				d.leaseCloseTimeout, plan.leaseObjectURI,
			)
		}

		writeCtx, cancel := context.WithTimeout(ctx, remaining)
		updated, err := store.UpdateLease(writeCtx, plan.leaseKey, toWrite)
		cancel()
		if err == nil {
			d.logf("lease: closed, no cluster can take it")
			return prior, updated, true, nil
		}
		if store.IsConflict(err) {
			if d.leaseRetryInterval > 0 {
				sleep := d.leaseRetryInterval
				if rem := deadline.Sub(d.nowFunc()); sleep > rem {
					sleep = rem
				}
				if sleep > 0 {
					timer := time.NewTimer(sleep)
					select {
					case <-ctx.Done():
						timer.Stop()
						return mclstorage.LeaseData{}, nil, false, ctx.Err()
					case <-timer.C:
					}
				}
			}
			continue
		}
		if isLeasePermissionError(err) {
			return mclstorage.LeaseData{}, nil, false, leasePermissionError("writing", plan.bucket, plan.leaseObjectURI, err)
		}
		return mclstorage.LeaseData{}, nil, false, fmt.Errorf("writing lease object %s: %w", plan.leaseObjectURI, err)
	}
}

func (d *deactivator) restoreLease(ctx context.Context, store mclstorage.Storage, plan *deactivatePlan, prior mclstorage.LeaseData, closedRV string) error {
	_, err := store.UpdateLease(ctx, plan.leaseKey, &mclstorage.LeaseObject{
		ResourceVersion: closedRV,
		Data:            prior,
	})
	return err
}

// makeStandalone implements Step 2: update X's ConfigConnector to remove
// spec.experiments.multiClusterLease and store it in deactivatedLeaseAnnotation, retrying on
// Kubernetes resourceVersion conflicts.
func (d *deactivator) makeStandalone(ctx context.Context) error {
	var lastErr error
	for attempt := 0; attempt < maxConfigConnectorUpdateRetries; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, d.clusterOpTimeout)
		cc, err := fetchConfigConnector(callCtx, d.standalone.handle)
		if err != nil {
			cancel()
			return err
		}
		if isStandaloneConfigConnector(cc) {
			cancel()
			return nil
		}
		if err := deactivateConfigConnector(cc); err != nil {
			cancel()
			return err
		}
		err = d.standalone.handle.client.Update(callCtx, cc)
		cancel()
		if err == nil {
			return nil
		}
		if apierrors.IsConflict(err) {
			lastErr = err
			continue
		}
		return fmt.Errorf("updating ConfigConnector %q on cluster %q: %w", cc.Name, d.standalone.name(), err)
	}
	return fmt.Errorf("updating ConfigConnector %q on cluster %q failed after %d conflict retries: %w", configConnectorResourceName, d.standalone.name(), maxConfigConnectorUpdateRetries, lastErr)
}

// parkOtherClusters implements Step 3 for every cluster in --contexts other than X.
func (d *deactivator) parkOtherClusters(ctx context.Context) (parkErrs []error, interrupted error) {
	for _, c := range d.clusters {
		if c == d.standalone {
			continue
		}
		if err := ctx.Err(); err != nil {
			return parkErrs, fmt.Errorf("interrupted: %w", err)
		}
		if err := d.parkCluster(ctx, c); err != nil {
			if ctx.Err() != nil {
				return parkErrs, fmt.Errorf("interrupted: %w", ctx.Err())
			}
			parkErrs = append(parkErrs, fmt.Errorf(
				"multi-cluster HA is deactivated, but cluster %q is not stopped: %w. It cannot lead, because the lease is closed. Run the command again.",
				c.name(), err,
			))
		}
		if d.onAfterStep != nil {
			if err := d.onAfterStep("step3:" + c.name()); err != nil {
				return parkErrs, err
			}
		}
	}
	return parkErrs, nil
}

func (d *deactivator) parkCluster(ctx context.Context, c *deactivateCluster) error {
	// 1. Scale every manager StatefulSet to 0 with deactivatedReplicasAnnotation.
	callCtx, cancel := context.WithTimeout(ctx, d.clusterOpTimeout)
	waiting, err := c.handle.stopManagers(callCtx, deactivatedReplicasAnnotation, d.out, func(m *managerStatefulSet) {
		d.logf("cluster %q: StatefulSet %s is already scaled to 0 without %s; leaving it alone (ha activate will not start it)", c.name(), m.key(), deactivatedReplicasAnnotation)
	})
	cancel()
	if err != nil {
		return err
	}

	// 2. Wait until its manager pods are gone.
	if waiting {
		d.logf("cluster %q: waiting up to %s for its manager pods to stop", c.name(), d.timeout)
		err = poll(ctx, d.pollInterval, d.timeout, func(pCtx context.Context) (string, error) {
			return c.handle.pendingStop(pCtx, "deactivate")
		})
		if err != nil {
			return fmt.Errorf("Config Connector's manager did not stop: %w", err)
		}
	}

	// 3. Suspend the KRMSyncers of the lease (after pods are gone so a standby manager cannot resume them).
	suspended, err := d.suspendLeaseSyncers(ctx, c)
	if err != nil {
		return err
	}

	// 4. Scale the MultiClusterLease controller Deployment to 0 with the same annotation and wait until its pod is gone.
	stoppedMCL, err := d.stopMCLController(ctx, c)
	if err != nil {
		return err
	}

	switch {
	case suspended > 0 && stoppedMCL:
		d.logf("cluster %q: suspended %d %s, stopped the MultiClusterLease controller", c.name(), suspended, pluralSyncer(suspended))
	case suspended > 0:
		d.logf("cluster %q: suspended %d %s", c.name(), suspended, pluralSyncer(suspended))
	case stoppedMCL:
		d.logf("cluster %q: stopped the MultiClusterLease controller", c.name())
	}
	return nil
}

func (d *deactivator) suspendLeaseSyncers(ctx context.Context, c *deactivateCluster) (int, error) {
	callCtx, cancel := context.WithTimeout(ctx, d.clusterOpTimeout)
	defer cancel()

	allSyncers, err := listKRMSyncers(callCtx, c.handle, d.leaseNS)
	if err != nil {
		return 0, err
	}
	suspended := 0
	for i := range allSyncers {
		s := &allSyncers[i]
		if s.Name != d.leaseName && !strings.HasPrefix(s.Name, d.leaseName+"-") {
			continue
		}
		if s.Spec.Suspend {
			continue
		}
		if err := c.handle.client.Patch(callCtx, s, suspendSyncerPatch); err != nil {
			return suspended, fmt.Errorf("suspending KRMSyncer %s/%s on cluster %q: %w", s.Namespace, s.Name, c.name(), err)
		}
		suspended++
	}
	return suspended, nil
}

func (d *deactivator) ensureStandaloneSyncersSuspended(ctx context.Context) error {
	suspended, err := d.suspendLeaseSyncers(ctx, d.standalone)
	if err != nil {
		return fmt.Errorf("suspending KRMSyncers on standalone cluster %q: %w", d.standalone.name(), err)
	}
	if suspended > 0 {
		d.logf("cluster %q: suspended %d %s", d.standalone.name(), suspended, pluralSyncer(suspended))
	}
	return nil
}

func (d *deactivator) stopMCLController(ctx context.Context, c *deactivateCluster) (bool, error) {
	key := types.NamespacedName{Namespace: manifestSystemNamespace, Name: mclDeploymentName}
	callCtx, cancel := context.WithTimeout(ctx, d.clusterOpTimeout)
	dep := &appsv1.Deployment{}
	err := c.handle.client.Get(callCtx, key, dep)
	cancel()
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("fetching Deployment %s on cluster %q: %w", key, c.name(), err)
	}

	replicas := deploymentReplicas(dep)
	var stopped, waiting bool
	if replicas > 0 {
		ann := strconv.Itoa(int(replicas))
		patch, err := managerPatch(deactivatedReplicasAnnotation, 0, &ann)
		if err != nil {
			return false, err
		}
		patchCtx, cancel := context.WithTimeout(ctx, d.clusterOpTimeout)
		err = c.handle.client.Patch(patchCtx, dep, patch)
		cancel()
		if err != nil {
			return false, fmt.Errorf("stopping Deployment %s on cluster %q: %w", key, c.name(), err)
		}
		stopped = true
		waiting = true
	} else if deploymentPendingStopped(dep) != "" {
		waiting = true
	}

	if waiting {
		err = poll(ctx, d.pollInterval, d.timeout, func(pCtx context.Context) (string, error) {
			cur := &appsv1.Deployment{}
			if err := c.handle.client.Get(pCtx, key, cur); err != nil {
				if apierrors.IsNotFound(err) {
					return "", nil
				}
				return err.Error(), nil
			}
			if deploymentReplicas(cur) > 0 {
				return "", fmt.Errorf("Deployment %s runs again: something scaled it up after deactivate stopped it", key)
			}
			return deploymentPendingStopped(cur), nil
		})
		if err != nil {
			return stopped, fmt.Errorf("Deployment %s did not stop: %w", key, err)
		}
	}
	return stopped, nil
}

type managerPodRecord struct {
	uid          types.UID
	restartCount int32
	deleted      bool
}

func (d *deactivator) recordStandaloneManagerPods(ctx context.Context) (map[string]map[string]*managerPodRecord, error) {
	managers, err := d.standalone.handle.listManagers(ctx)
	if err != nil {
		return nil, err
	}
	pods, err := listManagerPods(ctx, d.standalone.handle)
	if err != nil {
		return nil, err
	}
	snapshot := make(map[string]map[string]*managerPodRecord, len(managers))
	for _, m := range managers {
		matched, err := podsForStatefulSet(m, pods)
		if err != nil {
			return nil, err
		}
		perPod := make(map[string]*managerPodRecord, len(matched))
		for i := range matched {
			p := &matched[i]
			perPod[p.Name] = &managerPodRecord{
				uid:          p.UID,
				restartCount: managerContainerRestartCount(p),
			}
		}
		snapshot[m.key()] = perPod
	}
	return snapshot, nil
}

// waitForStandalone implements Step 4: wait until every manager StatefulSet of X has ready pods
// running without leader election.
func (d *deactivator) waitForStandalone(ctx context.Context, needRestart bool, snapshot map[string]map[string]*managerPodRecord, since time.Time) error {
	if !needRestart {
		// X was already standalone and the lease was already closed before this run; just ensure
		// its managers are ready without forcing any restarts or deletions.
		ready, pending, err := d.standaloneManagersReady(ctx)
		if err == nil && ready {
			d.logf("cluster %q: reconciles without leader election", d.standalone.name())
			return nil
		}
		d.logf("cluster %q: waiting up to %s for its managers to be ready without leader election", d.standalone.name(), d.timeout)
		_ = pending
		err = poll(ctx, d.pollInterval, d.timeout, func(pCtx context.Context) (string, error) {
			ok, p, checkErr := d.standaloneManagersReady(pCtx)
			if checkErr != nil {
				return checkErr.Error(), nil
			}
			if ok {
				return "", nil
			}
			return p, nil
		})
		if err != nil {
			return fmt.Errorf("cluster %q: managers are not ready: %w", d.standalone.name(), err)
		}
		d.logf("cluster %q: reconciles without leader election", d.standalone.name())
		return nil
	}

	d.logf("cluster %q: waiting up to %s for its managers to restart without leader election", d.standalone.name(), d.timeout)
	err := poll(ctx, d.pollInterval, d.timeout, func(pCtx context.Context) (string, error) {
		managers, err := d.standalone.handle.listManagers(pCtx)
		if err != nil {
			return err.Error(), nil
		}
		pods, err := listManagerPods(pCtx, d.standalone.handle)
		if err != nil {
			return err.Error(), nil
		}
		deleteDue := d.nowFunc().Sub(since) >= d.podDeleteDelay
		for _, m := range managers {
			want := m.replicas()
			if want == 0 {
				continue
			}
			matched, err := podsForStatefulSet(m, pods)
			if err != nil {
				return "", err
			}
			recordedPods := snapshot[m.key()]
			var readyFresh int32
			var stalePods []*corev1.Pod
			for i := range matched {
				p := &matched[i]
				rec, existed := recordedPods[p.Name]
				restartedOrReplaced := !existed || p.UID != rec.uid || managerContainerRestartCount(p) > rec.restartCount
				if restartedOrReplaced {
					if isPodReady(p) {
						readyFresh++
					}
				} else if p.DeletionTimestamp == nil {
					stalePods = append(stalePods, p)
				}
			}
			if readyFresh >= want {
				continue
			}
			if deleteDue {
				for _, p := range stalePods {
					rec := recordedPods[p.Name]
					if rec != nil && !rec.deleted {
						if err := d.standalone.handle.client.Delete(pCtx, p); err != nil && !apierrors.IsNotFound(err) {
							return fmt.Sprintf("deleting stale manager pod %s/%s on cluster %q failed: %v", p.Namespace, p.Name, d.standalone.name(), err), nil
						}
						rec.deleted = true
					}
				}
			}
			return fmt.Sprintf("StatefulSet %s has %d of %d ready pods restarted without leader election", m.key(), readyFresh, want), nil
		}
		return "", nil
	})
	if err != nil {
		return fmt.Errorf("cluster %q did not restart its managers without leader election: %w", d.standalone.name(), err)
	}
	d.logf("cluster %q: reconciles without leader election", d.standalone.name())
	return nil
}

func (d *deactivator) standaloneManagersReady(ctx context.Context) (bool, string, error) {
	managers, err := d.standalone.handle.listManagers(ctx)
	if err != nil {
		return false, "", err
	}
	pods, err := listManagerPods(ctx, d.standalone.handle)
	if err != nil {
		return false, "", err
	}
	for _, m := range managers {
		want := m.replicas()
		if want == 0 {
			continue
		}
		if pending := m.pendingReady(); pending != "" {
			return false, pending, nil
		}
		matched, err := podsForStatefulSet(m, pods)
		if err != nil {
			return false, "", err
		}
		var ready int32
		for i := range matched {
			if isPodReady(&matched[i]) {
				ready++
			}
		}
		if ready < want {
			return false, fmt.Sprintf("StatefulSet %s has %d of %d pods ready", m.key(), ready, want), nil
		}
	}
	return true, "", nil
}

func (d *deactivator) printReport() {
	d.logf("Multi-cluster HA is deactivated: %s reconciles on its own.", d.standalone.name())

	var others []string
	var allContexts []string
	for _, c := range d.clusters {
		allContexts = append(allContexts, c.name())
		if c != d.standalone {
			others = append(others, c.name())
		}
	}
	if len(others) == 1 {
		d.logf("- %s keeps its copies of the resources and its lease configuration. Leave both in place:\nwith them, a manager that starts on %s cannot lead.", others[0], others[0])
	} else if len(others) > 1 {
		d.logf("- %s keep their copies of the resources and their lease configuration. Leave both in place:\nwith them, a manager that starts on them cannot lead.", strings.Join(others, ", "))
	}
	d.logf("- If GitOps applies %s's ConfigConnector, remove spec.experiments.multiClusterLease from\nits source now. Otherwise it is added back and %s stops reconciling.", d.standalone.name(), d.standalone.name())
	d.logf("- The HA components and the lease bucket are still installed.")
	d.logf("- To turn HA on again: config-connector ha activate --%s=%s", contextsFlag, strings.Join(allContexts, ","))
}

func listManagerPods(ctx context.Context, h *clusterHandle) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	labels := client.MatchingLabels{operatork8s.KCCSystemComponentLabel: operatork8s.KCCControllerManagerComponent}
	if err := h.client.List(ctx, list, labels); err != nil {
		return nil, fmt.Errorf("listing manager pods on cluster %q: %w", h.contextName, err)
	}
	return list.Items, nil
}

func podsForStatefulSet(m *managerStatefulSet, pods []corev1.Pod) ([]corev1.Pod, error) {
	if m.Spec.Selector == nil {
		return nil, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(m.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("invalid selector on StatefulSet %s: %w", m.key(), err)
	}
	var matched []corev1.Pod
	for _, pod := range pods {
		if pod.Namespace == m.Namespace && selector.Matches(labels.Set(pod.Labels)) {
			matched = append(matched, pod)
		}
	}
	return matched, nil
}

func managerContainerRestartCount(pod *corev1.Pod) int32 {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "manager" {
			return cs.RestartCount
		}
	}
	return 0
}

func isPodReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func countUnsuspendedSyncers(syncers []syncerv1alpha1.KRMSyncer) int {
	n := 0
	for i := range syncers {
		if !syncers[i].Spec.Suspend {
			n++
		}
	}
	return n
}

func pluralSyncer(n int) string {
	if n == 1 {
		return "KRMSyncer"
	}
	return "KRMSyncers"
}

func isLeasePermissionError(err error) bool {
	var gErr *googleapi.Error
	if errors.As(err, &gErr) {
		return gErr.Code == http.StatusForbidden || gErr.Code == http.StatusUnauthorized
	}
	return false
}

func leasePermissionError(op, bucket, objectURI string, err error) error {
	return fmt.Errorf(
		"permission denied %s lease object %s in bucket %q (ensure your account can read and overwrite objects in the bucket, e.g. with roles/storage.objectAdmin); nothing was changed: %w",
		op, objectURI, bucket, err,
	)
}
