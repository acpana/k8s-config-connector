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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func deactivatedHAFixtures() haFixtures {
	fs := healthyHAFixtures()

	// Cluster A: standalone (removed multiClusterLease, added deactivated-lease annotation,
	// closed lease status with stale-candidate BackendHealthy=False as MCL controller writes it).
	_ = deactivateConfigConnector(fs[0].cc)
	closedHolder := closedLeaseHolder(testIdentityA)
	closedRenew := deactivatedLeaseRenewTime.Format(time.RFC3339)
	fs[0].lease.Status.GlobalHolderIdentity = &closedHolder
	fs[0].lease.Status.GlobalRenewTime = &closedRenew
	fs[0].lease.Status.Conditions = []metav1.Condition{
		{
			Type:    string(mclv1alpha1.ConditionTypeBackendHealthy),
			Status:  metav1.ConditionFalse,
			Reason:  "ReconcileFailed",
			Message: "candidate lease is stale",
		},
	}

	// Cluster B: parked (keeps multiClusterLease; managers and MCL Deployment scaled to 0 with
	// deactivated-replicas annotation; KRMSyncer suspended).
	for _, sts := range fs[1].managers {
		sts.Spec.Replicas = ptrInt32(0)
		sts.Status.Replicas = 0
		sts.Status.ReadyReplicas = 0
		if sts.Annotations == nil {
			sts.Annotations = map[string]string{}
		}
		sts.Annotations[deactivatedReplicasAnnotation] = "1"
	}
	for _, s := range fs[1].syncers {
		s.Spec.Suspend = true
	}
	for _, obj := range fs[1].extraObjs {
		if dep, ok := obj.(*appsv1.Deployment); ok && dep.Name == mclDeploymentName {
			dep.Spec.Replicas = ptrInt32(0)
			dep.Status.Replicas = 0
			dep.Status.ReadyReplicas = 0
			dep.Status.AvailableReplicas = 0
			if dep.Annotations == nil {
				dep.Annotations = map[string]string{}
			}
			dep.Annotations[deactivatedReplicasAnnotation] = "1"
		}
	}
	return fs
}

func TestStatusDeactivated(t *testing.T) {
	t.Run("healthy deactivated group prints HA Status: DEACTIVATED", func(t *testing.T) {
		fs := deactivatedHAFixtures()
		report := executeStatus(context.Background(), fs.handles(t), testNow)
		var out bytes.Buffer
		report.render(&out)

		if report.state != haStateDeactivated {
			t.Fatalf("got state %q, want %q\noutput:\n%s", report.state, haStateDeactivated, out.String())
		}
		want := `CONTEXT                           PROJECT  LOCATION     HA           ROLE
gke_proj-a_us-central1_cluster-a  proj-a   us-central1  Deactivated  Standalone
gke_proj-b_us-east1_cluster-b     proj-b   us-east1     Deactivated  Parked

Lease: cnrm-system/kcc-multicluster-lease, closed for gke_proj-a_us-central1_cluster-a
HA Status: DEACTIVATED
`
		if got := out.String(); got != want {
			t.Fatalf("got output:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("parked cluster with stale pre-deactivation MultiClusterLease status is still DEACTIVATED", func(t *testing.T) {
		fs := deactivatedHAFixtures()
		// Cluster B's MCL controller was scaled to 0 before polling the closed lease, so its
		// MultiClusterLease.status still shows an old expired renewal for cluster-a.
		oldHolder := testIdentityA
		oldRenew := "2020-01-01T00:00:00Z"
		fs[1].lease.Status.GlobalHolderIdentity = &oldHolder
		fs[1].lease.Status.GlobalRenewTime = &oldRenew

		report := executeStatus(context.Background(), fs.handles(t), testNow)
		if report.state != haStateDeactivated {
			t.Fatalf("got state %q (problems=%v), want %q", report.state, report.problems, haStateDeactivated)
		}
	})

	t.Run("standalone cluster with single post-deactivation container restart is still DEACTIVATED", func(t *testing.T) {
		fs := deactivatedHAFixtures()
		for _, obj := range fs[0].extraObjs {
			if pod, ok := obj.(*corev1.Pod); ok && pod.Name == "cnrm-controller-manager-system-0" {
				pod.Status.ContainerStatuses[0].RestartCount = 1
				pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(testNow)
			}
		}

		report := executeStatus(context.Background(), fs.handles(t), testNow)
		if report.state != haStateDeactivated {
			t.Fatalf("got state %q (problems=%v), want %q", report.state, report.problems, haStateDeactivated)
		}
	})

	t.Run("closed without identity with one standalone cluster is DEACTIVATED", func(t *testing.T) {
		fs := deactivatedHAFixtures()
		bare := deactivatedLeaseHolder
		fs[0].lease.Status.GlobalHolderIdentity = &bare

		report := executeStatus(context.Background(), fs.handles(t), testNow)
		var out bytes.Buffer
		report.render(&out)
		if report.state != haStateDeactivated {
			t.Fatalf("got state %q, want %q\noutput:\n%s", report.state, haStateDeactivated, out.String())
		}
		if !strings.Contains(out.String(), "Lease: cnrm-system/kcc-multicluster-lease, closed\n") {
			t.Fatalf("expected bare closed lease summary:\n%s", out.String())
		}
	})

	errorCases := []struct {
		name        string
		mutate      func(fs haFixtures)
		wantProblem string
	}{
		{
			name: "lease closed but no cluster is standalone (died between step 1 and 2)",
			mutate: func(fs haFixtures) {
				// Restore active healthy fixtures and close only the lease.
				healthy := healthyHAFixtures()
				fs[0], fs[1] = healthy[0], healthy[1]
				closed := closedLeaseHolder(testIdentityA)
				closedRenew := deactivatedLeaseRenewTime.Format(time.RFC3339)
				fs[0].lease.Status.GlobalHolderIdentity = &closed
				fs[0].lease.Status.GlobalRenewTime = &closedRenew
				fs[1].lease.Status.GlobalHolderIdentity = &closed
				fs[1].lease.Status.GlobalRenewTime = &closedRenew
			},
			wantProblem: "no cluster is standalone",
		},
		{
			name: "standalone cluster but lease is not closed",
			mutate: func(fs haFixtures) {
				openHolder := testIdentityA
				fs[0].lease.Status.GlobalHolderIdentity = &openHolder
			},
			wantProblem: "is not closed",
		},
		{
			name: "lease closed for a different cluster than the standalone one",
			mutate: func(fs haFixtures) {
				wrongClosed := closedLeaseHolder(testIdentityB)
				fs[0].lease.Status.GlobalHolderIdentity = &wrongClosed
			},
			wantProblem: "is closed for",
		},
		{
			name: "two standalone clusters",
			mutate: func(fs haFixtures) {
				_ = deactivateConfigConnector(fs[1].cc)
			},
			wantProblem: "multiple clusters are standalone",
		},
		{
			name: "other cluster manager still running",
			mutate: func(fs haFixtures) {
				fs[1].managers[0].Spec.Replicas = ptrInt32(1)
				fs[1].managers[0].Status.Replicas = 1
			},
			wantProblem: "is still running",
		},
		{
			name: "other cluster KRMSyncer not suspended",
			mutate: func(fs haFixtures) {
				fs[1].syncers[0].Spec.Suspend = false
			},
			wantProblem: "is not suspended",
		},
		{
			name: "other cluster MCL Deployment still running",
			mutate: func(fs haFixtures) {
				for _, obj := range fs[1].extraObjs {
					if dep, ok := obj.(*appsv1.Deployment); ok && dep.Name == mclDeploymentName {
						dep.Spec.Replicas = ptrInt32(1)
						dep.Status.Replicas = 1
						dep.Status.AvailableReplicas = 1
					}
				}
			},
			wantProblem: `Deployment "multiclusterlease-controller-manager" is still running`,
		},
		{
			name: "GitOps re-added multiClusterLease alongside deactivated-lease annotation",
			mutate: func(fs haFixtures) {
				healthy := healthyHAFixtures()
				fs[0].cc.Spec.Experiments = healthy[0].cc.Spec.Experiments
			},
			wantProblem: "has both spec.experiments.multiClusterLease",
		},
		{
			name: "standalone cluster manager scaled to 0",
			mutate: func(fs haFixtures) {
				fs[0].managers[0].Spec.Replicas = ptrInt32(0)
				fs[0].managers[0].Status.Replicas = 0
				fs[0].managers[0].Status.ReadyReplicas = 0
			},
			wantProblem: "is not running",
		},
		{
			name: "standalone cluster KRMSyncer not suspended",
			mutate: func(fs haFixtures) {
				fs[0].syncers[0].Spec.Suspend = false
			},
			wantProblem: "(standalone): KRMSyncer",
		},
		{
			name: "invalid deactivated-lease annotation JSON",
			mutate: func(fs haFixtures) {
				fs[0].cc.Annotations[deactivatedLeaseAnnotation] = "{not-json"
			},
			wantProblem: "invalid " + deactivatedLeaseAnnotation,
		},
	}

	for _, tc := range errorCases {
		t.Run(tc.name, func(t *testing.T) {
			fs := deactivatedHAFixtures()
			tc.mutate(fs)

			report := executeStatus(context.Background(), fs.handles(t), testNow)
			var out bytes.Buffer
			report.render(&out)
			if report.state != haStateError {
				t.Fatalf("got state %q, want %q\noutput:\n%s", report.state, haStateError, out.String())
			}
			if !strings.Contains(out.String(), tc.wantProblem) {
				t.Fatalf("expected output to contain %q, got:\n%s", tc.wantProblem, out.String())
			}
		})
	}
}
