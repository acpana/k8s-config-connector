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
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	operatorv1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/operator/pkg/apis/core/v1beta1"
	operatork8s "github.com/GoogleCloudPlatform/k8s-config-connector/operator/pkg/k8s"
	mclv1alpha1 "github.com/gke-labs/multicluster-leader-election/api/v1alpha1"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// statusOptions holds flags and configuration for `config-connector ha status`.
type statusOptions struct {
	baseOptions

	bucket string
}

func newStatusOptions() statusOptions {
	return statusOptions{}
}

const statusLong = `Display multi-cluster high availability leadership, manager, and replication status.

Status inspects each cluster named in --contexts and checks:
  - the clusters share a consistent MultiClusterLease configuration;
  - each cluster's lease backend is healthy and the lease has a live holder;
  - all clusters agree on the current leader;
  - Config Connector's manager StatefulSets are running and ready;
  - KRMSyncers are suspended on the leader and actively syncing on standbys.

In namespaced mode, all managers on a cluster share the multi-cluster lease under the
cluster's identity, so leadership never moves for one namespace alone.`

func NewStatusCmd() *cobra.Command {
	opts := newStatusOptions()
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Display multi-cluster high availability leadership and replication status",
		Long:  statusLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.stdout = cmd.OutOrStdout()
			opts.stderr = cmd.ErrOrStderr()
			if err := opts.validate(); err != nil {
				return err
			}
			// From here on, errors are about the clusters, not about how the command was used.
			cmd.SilenceUsage = true
			return runStatus(cmd.Context(), opts)
		},
	}
	opts.addFlags(cmd.Flags())
	return cmd
}

func (o *statusOptions) addFlags(flags *pflag.FlagSet) {
	o.baseOptions.addFlags(flags)
	flags.StringVar(&o.bucket, bucketFlag, o.bucket, "Optional GCS bucket name to inspect the global lock object directly.")
}

func (o *statusOptions) validate() error {
	if err := o.baseOptions.validate(); err != nil {
		return err
	}
	// Like `ha setup`, require GKE contexts so that every cluster's project and location are known.
	var errs []error
	for _, contextName := range o.contexts {
		if _, err := parseGKEContext(contextName); err != nil {
			errs = append(errs, err)
		}
	}
	o.bucket = normalizeBucketName(o.bucket)
	return errors.Join(errs...)
}

// errHAStatusError is returned when the HA status is ERROR, so that the command exits non-zero.
var errHAStatusError = errors.New("multi-cluster HA status is ERROR; see the problems above")

func runStatus(ctx context.Context, opts statusOptions) error {
	handles, err := buildClusterHandles(ctx, opts.contexts)
	if err != nil {
		return err
	}

	report := executeStatus(ctx, handles, time.Now())

	stdout := opts.stdout
	if stdout == nil {
		stdout = io.Discard
	}
	report.render(stdout)

	if report.state == haStateError {
		return errHAStatusError
	}
	return nil
}

// executeStatus inspects every cluster and builds the HA status report. now is used to tell
// whether the lease has expired; it is a parameter so tests are deterministic.
func executeStatus(ctx context.Context, handles []*clusterHandle, now time.Time) *statusReport {
	clusters := make([]*clusterStatus, 0, len(handles))
	for _, h := range handles {
		clusters = append(clusters, inspectCluster(ctx, h))
	}
	return buildStatusReport(clusters, now)
}

// clusterHA is the value of the HA column for a cluster.
type clusterHA string

const (
	clusterHAConfigured        clusterHA = "Configured"
	clusterHADeactivated       clusterHA = "Deactivated"
	clusterHANotConfigured     clusterHA = "Not configured"
	clusterHANoConfigConnector clusterHA = "No ConfigConnector"
	clusterHAError             clusterHA = "Error"
)

// clusterRole is the value of the ROLE column for a cluster.
type clusterRole string

const (
	// roleLeader: the cluster holds the multi-cluster lease, so its Config Connector reconciles.
	roleLeader clusterRole = "Leader"
	// roleStandby: another cluster holds the lease; this cluster waits to take over.
	roleStandby clusterRole = "Standby"
	// roleStandalone: multi-cluster HA is deactivated and this cluster reconciles without leader election.
	roleStandalone clusterRole = "Standalone"
	// roleParked: multi-cluster HA is deactivated and this cluster's managers are stopped.
	roleParked clusterRole = "Parked"
	// roleUnknown: HA is configured, but the lease holder could not be determined.
	roleUnknown clusterRole = "Unknown"
	// roleNone: HA is not configured, or the cluster could not be inspected.
	roleNone clusterRole = "-"
)

// clusterStatus is the observed HA status of one cluster.
type clusterStatus struct {
	gke  gkeClusterInfo
	ha   clusterHA
	role clusterRole
	// state is the cluster's HA objects; set when HA is configured or deactivated on this cluster.
	state *clusterHAState
	// deactivatedLease holds the parsed spec.experiments.multiClusterLease saved in
	// deactivatedLeaseAnnotation when this cluster is standalone (no active multiClusterLease).
	deactivatedLease *operatorv1beta1.MultiClusterLeaseSpec
	// err is the error from inspecting the cluster; nil when HA is configured or standalone.
	err error
	// workloads holds the observed HA workloads on the cluster; set when HA is configured or standalone.
	workloads *clusterWorkloads
	// workloadsErr is non-nil if reading workloads failed with a non-NotFound error.
	workloadsErr error
}

// leaseSpec returns the cluster's MultiClusterLeaseSpec, whether active on spec.experiments or
// saved in deactivatedLeaseAnnotation.
func (c *clusterStatus) leaseSpec() *operatorv1beta1.MultiClusterLeaseSpec {
	if c.state != nil {
		if mcl := multiClusterLeaseSpec(c.state.configConnector); mcl != nil {
			return mcl
		}
	}
	return c.deactivatedLease
}

// isStandalone reports whether this cluster's ConfigConnector has deactivatedLeaseAnnotation and
// no active spec.experiments.multiClusterLease.
func (c *clusterStatus) isStandalone() bool {
	return c.ha == clusterHADeactivated && c.deactivatedLease != nil
}

// identity returns the cluster's identity in the election, or "" if HA is not configured or deactivated.
func (c *clusterStatus) identity() string {
	mcl := c.leaseSpec()
	if mcl == nil {
		return ""
	}
	identity, _ := mcl.CandidateIdentity()
	return identity
}

// statusClusterTimeout bounds how long a single cluster is inspected, so that one unreachable
// cluster cannot hang the command.
const statusClusterTimeout = 30 * time.Second

// inspectCluster fetches the cluster's HA state and maps the result to the HA column.
func inspectCluster(ctx context.Context, h *clusterHandle) *clusterStatus {
	gke, err := parseGKEContext(h.contextName)
	if err != nil {
		gke = gkeClusterInfo{contextName: h.contextName}
	}
	c := &clusterStatus{gke: gke, role: roleNone}

	ctx, cancel := context.WithTimeout(ctx, statusClusterTimeout)
	defer cancel()

	cc, err := fetchConfigConnector(ctx, h)
	if err != nil {
		c.err = err
		var notFound *configConnectorNotFoundError
		if errors.As(err, &notFound) {
			c.ha = clusterHANoConfigConnector
		} else {
			c.ha = clusterHAError
		}
		return c
	}

	mclSpec := multiClusterLeaseSpec(cc)
	if mclSpec != nil {
		if err := validateClusterLeaseSpec(h.contextName, mclSpec); err != nil {
			c.err = err
			c.ha = clusterHAError
			return c
		}
		lease, err := fetchMultiClusterLease(ctx, h, mclSpec.Namespace, mclSpec.LeaseName)
		if err != nil {
			c.err = err
			c.ha = clusterHAError
			return c
		}
		syncers, err := listKRMSyncers(ctx, h, mclSpec.Namespace)
		if err != nil {
			c.err = err
			c.ha = clusterHAError
			return c
		}
		c.ha = clusterHAConfigured
		c.state = &clusterHAState{
			contextName:       h.contextName,
			configConnector:   cc,
			multiClusterLease: lease,
			krmSyncers:        syncers,
		}
		c.workloads, c.workloadsErr = fetchClusterWorkloadsForLease(ctx, h, cc, mclSpec)
		return c
	}

	deactSpec, hasDeact, deactErr := deactivatedLeaseSpec(h.contextName, cc)
	if hasDeact {
		if deactErr != nil {
			c.err = deactErr
			c.ha = clusterHAError
			return c
		}
		lease, err := fetchMultiClusterLease(ctx, h, deactSpec.Namespace, deactSpec.LeaseName)
		if err != nil {
			c.err = err
			c.ha = clusterHAError
			return c
		}
		syncers, err := listKRMSyncers(ctx, h, deactSpec.Namespace)
		if err != nil {
			c.err = err
			c.ha = clusterHAError
			return c
		}
		c.ha = clusterHADeactivated
		c.deactivatedLease = deactSpec
		c.state = &clusterHAState{
			contextName:       h.contextName,
			configConnector:   cc,
			multiClusterLease: lease,
			krmSyncers:        syncers,
		}
		c.workloads, c.workloadsErr = fetchClusterWorkloadsForLease(ctx, h, cc, deactSpec)
		return c
	}

	c.err = &multiClusterLeaseNotConfiguredError{contextName: h.contextName}
	c.ha = clusterHANotConfigured
	return c
}

type haState string

const (
	haStateNotDetected haState = "NOT DETECTED"
	// haStateHealthy means HA is configured on every cluster and no problem was found.
	haStateHealthy haState = "HEALTHY"
	// haStateDeactivated means multi-cluster HA is deactivated: one cluster reconciles on its own
	// with the lease closed and the other clusters parked.
	haStateDeactivated haState = "DEACTIVATED"
	haStateError       haState = "ERROR"
)

// statusReport is the overall multi-cluster HA status across the inspected clusters.
type statusReport struct {
	state    haState
	clusters []*clusterStatus
	problems []string
	warnings []string
	// lease describes the multi-cluster lease; nil when its holder is not known.
	lease *leaseSummary
	now   time.Time
}

// leaseSummary describes the multi-cluster lease and the cluster that holds it (or for which it is closed).
type leaseSummary struct {
	// key is <namespace>/<name> of the MultiClusterLease.
	key string
	// holder is the context of the leader (or standalone cluster when closed), or its identity if
	// it's not in --contexts. Empty if closed without an identity.
	holder string
	// closed is true when the lease carries the deactivated holder marker.
	closed bool
	// renewTime is when the leader last renewed the lease; zero if not reported or closed.
	renewTime time.Time
}

func buildStatusReport(clusters []*clusterStatus, now time.Time) *statusReport {
	r := &statusReport{clusters: clusters, now: now}
	if hasDeactivationMarkers(clusters) {
		r.buildDeactivatedReport()
		return r
	}

	var configured, notConfigured []*clusterStatus
	for _, c := range clusters {
		switch c.ha {
		case clusterHAConfigured:
			configured = append(configured, c)
			c.role = roleUnknown
		case clusterHANotConfigured:
			notConfigured = append(notConfigured, c)
		default:
			r.problems = append(r.problems, c.err.Error())
		}
	}

	// HA must be configured on all clusters or on none of them. A cluster without HA runs its own
	// leader election, so it reconciles alongside whichever cluster holds the multi-cluster lease.
	if len(configured) > 0 {
		for _, c := range notConfigured {
			r.problems = append(r.problems, c.err.Error()+"; it reconciles independently of the HA clusters, so more than one cluster may change the same resources")
		}
	}

	// Check the election, leadership and data sync across the configured clusters, even if some
	// clusters aren't configured or couldn't be read.
	if len(configured) > 0 {
		states := make([]*clusterHAState, 0, len(configured))
		for _, c := range configured {
			states = append(states, c.state)
		}
		leader, problems := haProblems(states, r.contextIdentities(), now)
		r.problems = append(r.problems, problems...)
		if leader != "" {
			r.setLeader(leader, configured)
		}

		for _, c := range configured {
			if c.workloadsErr != nil {
				r.problems = append(r.problems, fmt.Sprintf("cluster %q: could not read Config Connector's workloads: %v", c.gke.contextName, c.workloadsErr))
				continue
			}
			if c.workloads != nil {
				probs, warns := c.workloads.checkWorkloads(c.gke.contextName, c.state, c.role == roleLeader, now)
				r.problems = append(r.problems, probs...)
				r.warnings = append(r.warnings, warns...)
			}
		}
	}

	switch {
	case len(r.problems) > 0:
		r.state = haStateError
	case len(configured) > 0:
		r.state = haStateHealthy
	default:
		r.state = haStateNotDetected
	}
	return r
}

// clusterIdentity returns a cluster's identity in the election: from its multiClusterLease spec
// when HA is configured, or else from its GKE context (gke_<project>_<location>_<cluster>), which
// is how `config-connector ha setup` sets it.
func clusterIdentity(c *clusterStatus) string {
	if id := c.identity(); id != "" {
		return id
	}
	gke := c.gke.gkeCluster()
	return gke.ResourceName()
}

// contextIdentities returns the election identity of every cluster in --contexts.
func (r *statusReport) contextIdentities() map[string]bool {
	ids := make(map[string]bool, len(r.clusters))
	for _, c := range r.clusters {
		ids[clusterIdentity(c)] = true
	}
	return ids
}

// setLeader sets each configured cluster's role, and the lease summary, from the identity of the
// cluster that holds the lease. The leader may be a cluster that isn't configured or couldn't be
// read, or one outside --contexts.
func (r *statusReport) setLeader(leader string, configured []*clusterStatus) {
	for _, c := range configured {
		if c.identity() == leader {
			c.role = roleLeader
		} else {
			c.role = roleStandby
		}
	}

	// Name the holder by its context when it's in --contexts, and by its identity otherwise.
	holder := leader
	for _, c := range r.clusters {
		if clusterIdentity(c) == leader {
			holder = c.gke.contextName
			break
		}
	}

	// haProblems checked that the configured clusters use the same lease.
	mcl := multiClusterLeaseSpec(configured[0].state.configConnector)
	r.lease = &leaseSummary{
		key:    mcl.Namespace + "/" + mcl.LeaseName,
		holder: holder,
	}
	// Use the latest renewal reported for the leader by any configured cluster.
	for _, c := range configured {
		lease := c.state.multiClusterLease
		if lease == nil || lease.Status.GlobalHolderIdentity == nil || *lease.Status.GlobalHolderIdentity != leader || lease.Status.GlobalRenewTime == nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339, *lease.Status.GlobalRenewTime); err == nil && t.After(r.lease.renewTime) {
			r.lease.renewTime = t
		}
	}
}

// render writes the report: a table with one row per cluster, then the overall status.
func (r *statusReport) render(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CONTEXT\tPROJECT\tLOCATION\tHA\tROLE")
	for _, c := range r.clusters {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.gke.contextName, c.gke.project, c.gke.location, c.ha, c.role)
	}
	tw.Flush()

	fmt.Fprintln(w)
	if r.lease != nil {
		if r.lease.closed {
			if r.lease.holder != "" {
				fmt.Fprintf(w, "Lease: %s, closed for %s\n", r.lease.key, r.lease.holder)
			} else {
				fmt.Fprintf(w, "Lease: %s, closed\n", r.lease.key)
			}
		} else {
			fmt.Fprintf(w, "Lease: %s, held by %s", r.lease.key, r.lease.holder)
			if !r.lease.renewTime.IsZero() {
				fmt.Fprintf(w, " (renewed %s ago)", r.now.Sub(r.lease.renewTime).Round(time.Second))
			}
			fmt.Fprintln(w)
		}
	}
	fmt.Fprintf(w, "HA Status: %s\n", r.state)
	switch r.state {
	case haStateNotDetected:
		contexts := make([]string, 0, len(r.clusters))
		for _, c := range r.clusters {
			contexts = append(contexts, c.gke.contextName)
		}
		fmt.Fprintf(w, "Next step: config-connector ha setup --%s=%s --%s=<BUCKET>\n", contextsFlag, strings.Join(contexts, ","), bucketFlag)
	case haStateError:
		fmt.Fprintln(w, "Problems:")
		for _, p := range r.problems {
			fmt.Fprintf(w, "  - %s\n", p)
		}
	}
	if len(r.warnings) > 0 {
		fmt.Fprintln(w, "Warnings:")
		for _, warn := range r.warnings {
			fmt.Fprintf(w, "  - %s\n", warn)
		}
	}
}

func hasDeactivationMarkers(clusters []*clusterStatus) bool {
	for _, c := range clusters {
		if c.isStandalone() {
			return true
		}
		if c.state != nil && c.state.configConnector != nil {
			if _, ok := c.state.configConnector.Annotations[deactivatedLeaseAnnotation]; ok {
				return true
			}
		}
		if c.workloads != nil {
			for _, mw := range c.workloads.managers {
				if hasDeactivatedReplicas(mw.sts.StatefulSet) {
					return true
				}
			}
			if c.workloads.mcl != nil && hasDeactivatedReplicas(c.workloads.mcl.deployment) {
				return true
			}
		}
		if c.state != nil && c.state.multiClusterLease != nil && c.state.multiClusterLease.Status.GlobalHolderIdentity != nil {
			if _, closed := parseClosedLeaseHolder(*c.state.multiClusterLease.Status.GlobalHolderIdentity); closed {
				return true
			}
		}
	}
	return false
}

func (r *statusReport) buildDeactivatedReport() {
	var standalone, configured []*clusterStatus
	for _, c := range r.clusters {
		switch c.ha {
		case clusterHADeactivated:
			standalone = append(standalone, c)
			c.role = roleStandalone
		case clusterHAConfigured:
			configured = append(configured, c)
			c.role = roleUnknown
		case clusterHANotConfigured:
			r.problems = append(r.problems, c.err.Error()+"; it reconciles independently of the HA group, so more than one cluster may change the same resources")
		default:
			r.problems = append(r.problems, c.err.Error())
		}
	}

	participating := make([]*clusterStatus, 0, len(standalone)+len(configured))
	participating = append(participating, standalone...)
	participating = append(participating, configured...)

	var leaseKey string
	if len(participating) > 0 {
		specs := make([]namedLeaseSpec, 0, len(participating))
		for _, c := range participating {
			specs = append(specs, namedLeaseSpec{contextName: c.gke.contextName, spec: c.leaseSpec()})
		}
		if err := validateMultiClusterLeaseSpecs(specs); err != nil {
			r.problems = append(r.problems, splitErrorLines(err)...)
		} else {
			mcl := participating[0].leaseSpec()
			leaseKey = mcl.Namespace + "/" + mcl.LeaseName
		}
	}

	var standaloneCluster *clusterStatus
	switch len(standalone) {
	case 0:
		r.problems = append(r.problems, `multi-cluster HA is partially deactivated and no cluster is standalone: no cluster reconciles; run "config-connector ha deactivate" again`)
	case 1:
		standaloneCluster = standalone[0]
	default:
		names := make([]string, 0, len(standalone))
		for _, c := range standalone {
			names = append(names, c.gke.contextName)
		}
		r.problems = append(r.problems, fmt.Sprintf("multiple clusters are standalone (%s) and reconcile independently without leader election", strings.Join(names, ", ")))
	}

	// Check lease status on every participating cluster whose MCL controller is still running.
	var closedSeen bool
	var closedHolderID string
	for _, c := range participating {
		if c.workloads != nil && c.workloads.mcl != nil && deploymentReplicas(c.workloads.mcl.deployment) == 0 {
			// This cluster's MCL controller was scaled to 0 when parking it, so its
			// MultiClusterLease status is no longer updated.
			continue
		}
		mcl := c.leaseSpec()
		lease := c.state.multiClusterLease
		if lease == nil {
			r.problems = append(r.problems, fmt.Sprintf("cluster %q: MultiClusterLease %s/%s not found; check that the multiclusterlease-controller-manager is running", c.gke.contextName, mcl.Namespace, mcl.LeaseName))
			continue
		}
		var holder string
		if lease.Status.GlobalHolderIdentity != nil {
			holder = *lease.Status.GlobalHolderIdentity
		}
		if cond := meta.FindStatusCondition(lease.Status.Conditions, string(mclv1alpha1.ConditionTypeBackendHealthy)); cond != nil && cond.Status == metav1.ConditionFalse {
			isExpectedStale := isStandbyStaleCandidate(cond, c.identity(), holder) || (c.isStandalone() && strings.Contains(cond.Message, staleCandidateMessage))
			if !isExpectedStale {
				r.problems = append(r.problems, fmt.Sprintf("cluster %q: the lease backend is unhealthy: %s", c.gke.contextName, cond.Message))
				continue
			}
		}
		closedID, isClosed := parseClosedLeaseHolder(holder)
		if !isClosed {
			if holder == "" {
				r.problems = append(r.problems, fmt.Sprintf("cluster %q: MultiClusterLease %s/%s has no holder; the lease is not closed", c.gke.contextName, mcl.Namespace, mcl.LeaseName))
			} else {
				r.problems = append(r.problems, fmt.Sprintf("cluster %q: the lease is held by %q and is not closed; run \"config-connector ha deactivate\" again to close it", c.gke.contextName, holder))
			}
			continue
		}
		if standaloneCluster != nil && closedID != "" && closedID != standaloneCluster.identity() {
			r.problems = append(r.problems, fmt.Sprintf("cluster %q: the lease is closed for %q, but cluster %q (%s) is standalone", c.gke.contextName, closedID, standaloneCluster.gke.contextName, standaloneCluster.identity()))
			continue
		}
		closedSeen = true
		if closedID != "" {
			closedHolderID = closedID
		}
	}

	if leaseKey != "" && closedSeen {
		holderLabel := closedHolderID
		if closedHolderID != "" {
			for _, c := range r.clusters {
				if clusterIdentity(c) == closedHolderID {
					holderLabel = c.gke.contextName
					break
				}
			}
		}
		r.lease = &leaseSummary{
			key:    leaseKey,
			holder: holderLabel,
			closed: true,
		}
	}

	// Check standalone cluster(s): managers running and ready, KRMSyncers suspended.
	for _, c := range standalone {
		mcl := c.leaseSpec()
		if replicationMode(mcl) != replicationModeDisabled {
			for _, syncer := range leaseSyncers(c.state.krmSyncers, mcl.LeaseName) {
				if !syncer.Spec.Suspend {
					r.problems = append(r.problems, fmt.Sprintf("cluster %q (standalone): KRMSyncer %s/%s is not suspended", c.gke.contextName, syncer.Namespace, syncer.Name))
				}
			}
		}
		if c.workloadsErr != nil {
			r.problems = append(r.problems, fmt.Sprintf("cluster %q: could not read Config Connector's workloads: %v", c.gke.contextName, c.workloadsErr))
			continue
		}
		if c.workloads != nil {
			probs, warns := checkStandaloneWorkloads(c.gke.contextName, c.state, mcl, c.workloads, r.now)
			r.problems = append(r.problems, probs...)
			r.warnings = append(r.warnings, warns...)
		}
	}

	// Check configured (non-standalone) clusters: they should be parked.
	for _, c := range configured {
		mcl := c.leaseSpec()
		var clusterProblems []string
		if _, hasDeactAnn := c.state.configConnector.Annotations[deactivatedLeaseAnnotation]; hasDeactAnn {
			clusterProblems = append(clusterProblems, fmt.Sprintf("cluster %q: ConfigConnector has both spec.experiments.multiClusterLease and %s; remove spec.experiments.multiClusterLease or run \"config-connector ha deactivate\" again", c.gke.contextName, deactivatedLeaseAnnotation))
		}
		if replicationMode(mcl) != replicationModeDisabled {
			for _, syncer := range leaseSyncers(c.state.krmSyncers, mcl.LeaseName) {
				if !syncer.Spec.Suspend {
					clusterProblems = append(clusterProblems, fmt.Sprintf("cluster %q: KRMSyncer %s/%s is not suspended; run \"config-connector ha deactivate\" again", c.gke.contextName, syncer.Namespace, syncer.Name))
				}
			}
		}
		if c.workloadsErr != nil {
			clusterProblems = append(clusterProblems, fmt.Sprintf("cluster %q: could not read Config Connector's workloads: %v", c.gke.contextName, c.workloadsErr))
		} else if c.workloads != nil {
			for _, mw := range c.workloads.managers {
				if mw.sts.replicas() > 0 {
					clusterProblems = append(clusterProblems, fmt.Sprintf("cluster %q: StatefulSet %s is still running; run \"config-connector ha deactivate\" again to park it", c.gke.contextName, mw.sts.key()))
				} else if mw.sts.Status.Replicas > 0 {
					clusterProblems = append(clusterProblems, fmt.Sprintf("cluster %q: StatefulSet %s still has pods (%d)", c.gke.contextName, mw.sts.key(), mw.sts.Status.Replicas))
				} else if _, hasFailover := mw.sts.Annotations[failoverReplicasAnnotation]; hasFailover {
					clusterProblems = append(clusterProblems, fmt.Sprintf("cluster %q: StatefulSet %s was stopped by ha failover; run \"config-connector ha failover\" again", c.gke.contextName, mw.sts.key()))
				}
			}
			if c.workloads.mcl != nil {
				if deploymentReplicas(c.workloads.mcl.deployment) > 0 {
					clusterProblems = append(clusterProblems, fmt.Sprintf("cluster %q: Deployment %q is still running; run \"config-connector ha deactivate\" again to park it", c.gke.contextName, c.workloads.mcl.deployment.Name))
				} else if c.workloads.mcl.deployment.Status.Replicas > 0 {
					clusterProblems = append(clusterProblems, fmt.Sprintf("cluster %q: Deployment %q still has pods (%d)", c.gke.contextName, c.workloads.mcl.deployment.Name, c.workloads.mcl.deployment.Status.Replicas))
				}
			}
		}
		if len(clusterProblems) == 0 {
			c.ha = clusterHADeactivated
			c.role = roleParked
		} else {
			if standaloneCluster != nil {
				c.role = roleStandby
			}
			r.problems = append(r.problems, clusterProblems...)
		}
	}

	if len(r.problems) > 0 {
		r.state = haStateError
	} else {
		r.state = haStateDeactivated
	}
}

func checkStandaloneWorkloads(contextName string, state *clusterHAState, mcl *operatorv1beta1.MultiClusterLeaseSpec, cw *clusterWorkloads, now time.Time) ([]string, []string) {
	var problems, warnings []string
	if cw.roleForbiddenErr != nil {
		warnings = append(warnings, fmt.Sprintf("cluster %q: could not read Role %q in namespace %q: %v", contextName, managerHARoleName, mcl.Namespace, cw.roleForbiddenErr))
	}
	if len(cw.managers) == 0 && state.configConnector.GetMode() != operatork8s.NamespacedMode {
		if cw.isHASetupManaged {
			problems = append(problems, fmt.Sprintf("cluster %q (standalone): no StatefulSet of Config Connector's manager found; nothing reconciles", contextName))
		} else {
			warnings = append(warnings, fmt.Sprintf("cluster %q: Config Connector's manager StatefulSet not found", contextName))
		}
	}
	reportedWorkloads := make(map[any]bool)
	for _, mw := range cw.managers {
		if prob, reported := checkManager(contextName, mw, true); reported {
			problems = append(problems, prob)
			reportedWorkloads[mw.sts.StatefulSet] = true
		}
	}
	if cw.mcl != nil {
		if prob, reported := checkDeployment(contextName, cw.mcl); reported {
			problems = append(problems, prob)
			reportedWorkloads[cw.mcl.deployment] = true
		}
	}
	if cw.syncer != nil && replicationMode(mcl) != replicationModeDisabled {
		if prob, reported := checkDeployment(contextName, cw.syncer); reported {
			problems = append(problems, prob)
			reportedWorkloads[cw.syncer.deployment] = true
		}
	}
	// On a standalone cluster right after deactivation, a manager container may have restarted once
	// when it lost the closed lease; only flag repeated restarts (>1) on manager pods, while
	// checking MCL and Syncer deployments normally.
	for _, mw := range cw.managers {
		if !reportedWorkloads[mw.sts.StatefulSet] {
			for _, pod := range mw.pods {
				for _, cs := range pod.Status.ContainerStatuses {
					if cs.RestartCount > 1 {
						problems = append(problems, checkRestartedContainers(contextName, []corev1.Pod{pod}, now)...)
						break
					}
				}
			}
		}
	}
	if cw.mcl != nil && !reportedWorkloads[cw.mcl.deployment] {
		problems = append(problems, checkRestartedContainers(contextName, cw.mcl.pods, now)...)
	}
	if cw.syncer != nil && replicationMode(mcl) != replicationModeDisabled && !reportedWorkloads[cw.syncer.deployment] {
		problems = append(problems, checkRestartedContainers(contextName, cw.syncer.pods, now)...)
	}
	return problems, warnings
}
