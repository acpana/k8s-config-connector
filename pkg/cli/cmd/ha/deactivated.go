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
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	operatorv1beta1 "github.com/GoogleCloudPlatform/k8s-config-connector/operator/pkg/apis/core/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// deactivatedLeaseAnnotation stores the removed spec.experiments.multiClusterLease block as
	// JSON on the standalone cluster's ConfigConnector when multi-cluster HA is deactivated.
	deactivatedLeaseAnnotation = "cnrm.cloud.google.com/ha-deactivated-lease"

	// deactivatedReplicasAnnotation marks a manager StatefulSet or the MultiClusterLease
	// controller's Deployment that `ha deactivate` scaled to 0. Its value is the replica count
	// that the workload had before deactivation.
	deactivatedReplicasAnnotation = "cnrm.cloud.google.com/ha-deactivated-replicas"

	// deactivatedLeaseHolder is the base holderIdentity written to a closed lease object in GCS.
	// When the standalone cluster's identity is known, the holder has the form
	// "config-connector-ha-deactivated:<identity>".
	deactivatedLeaseHolder       = "config-connector-ha-deactivated"
	deactivatedLeaseHolderPrefix = deactivatedLeaseHolder + ":"
)

// deactivatedLeaseRenewTime is the renewal timestamp written to a closed lease object in GCS so
// that no MultiClusterLease controller ever treats the lease as expired.
var deactivatedLeaseRenewTime = time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC)

// closedLeaseHolder returns the holderIdentity of a lease closed for the given standalone cluster
// identity (or the bare marker if identity is empty).
func closedLeaseHolder(identity string) string {
	if identity == "" {
		return deactivatedLeaseHolder
	}
	return deactivatedLeaseHolderPrefix + identity
}

// parseClosedLeaseHolder reports whether holder marks a closed lease, and returns the standalone
// cluster identity embedded in it (empty if the lease was closed without an identity).
func parseClosedLeaseHolder(holder string) (identity string, closed bool) {
	if holder == deactivatedLeaseHolder {
		return "", true
	}
	if strings.HasPrefix(holder, deactivatedLeaseHolderPrefix) {
		return strings.TrimPrefix(holder, deactivatedLeaseHolderPrefix), true
	}
	return "", false
}

// deactivatedLeaseSpec parses and validates the spec.experiments.multiClusterLease JSON saved in
// deactivatedLeaseAnnotation on cc. present is false if the annotation is absent.
func deactivatedLeaseSpec(contextName string, cc *operatorv1beta1.ConfigConnector) (spec *operatorv1beta1.MultiClusterLeaseSpec, present bool, err error) {
	if cc == nil {
		return nil, false, nil
	}
	raw, ok := cc.Annotations[deactivatedLeaseAnnotation]
	if !ok {
		return nil, false, nil
	}
	mcl := &operatorv1beta1.MultiClusterLeaseSpec{}
	if err := json.Unmarshal([]byte(raw), mcl); err != nil {
		return nil, true, fmt.Errorf("cluster %q: invalid %s annotation on ConfigConnector %q: %w", contextName, deactivatedLeaseAnnotation, cc.Name, err)
	}
	if err := validateClusterLeaseSpec(contextName, mcl); err != nil {
		return nil, true, fmt.Errorf("cluster %q: invalid %s annotation on ConfigConnector %q: %w", contextName, deactivatedLeaseAnnotation, cc.Name, err)
	}
	return mcl, true, nil
}

// isStandaloneConfigConnector reports whether cc has the deactivated-lease annotation and no active
// spec.experiments.multiClusterLease.
func isStandaloneConfigConnector(cc *operatorv1beta1.ConfigConnector) bool {
	if cc == nil || multiClusterLeaseSpec(cc) != nil {
		return false
	}
	_, ok := cc.Annotations[deactivatedLeaseAnnotation]
	return ok
}

// hasDeactivatedReplicas reports whether obj carries deactivatedReplicasAnnotation.
func hasDeactivatedReplicas(obj metav1.Object) bool {
	if obj == nil {
		return false
	}
	_, ok := obj.GetAnnotations()[deactivatedReplicasAnnotation]
	return ok
}

// parseReplicasAnnotation parses an integer replica count from annotationKey on obj.
func parseReplicasAnnotation(obj metav1.Object, annotationKey string) (replicas int32, annotated bool, err error) {
	if obj == nil {
		return 0, false, nil
	}
	value, annotated := obj.GetAnnotations()[annotationKey]
	if !annotated {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil || n < 0 {
		return 0, true, fmt.Errorf("%s/%s has the annotation %s=%q, which is not a number of replicas", obj.GetNamespace(), obj.GetName(), annotationKey, value)
	}
	return int32(n), true, nil
}

// deploymentReplicas returns the desired replica count of a Deployment, defaulting nil to 1.
func deploymentReplicas(d *appsv1.Deployment) int32 {
	if d == nil || d.Spec.Replicas == nil {
		return 1
	}
	return *d.Spec.Replicas
}

// deploymentPendingStopped returns what a Deployment scaled to 0 is still waiting for, or "" once
// its pods are gone.
func deploymentPendingStopped(d *appsv1.Deployment) string {
	switch {
	case d.Status.ObservedGeneration < d.Generation:
		return fmt.Sprintf("the Deployment controller has not processed the latest spec of Deployment %s/%s yet", d.Namespace, d.Name)
	case d.Status.Replicas > 0:
		return fmt.Sprintf("Deployment %s/%s still has pods (%d)", d.Namespace, d.Name, d.Status.Replicas)
	}
	return ""
}

// extractGCSBucketArg extracts and normalizes the --gcs-bucket argument from a Deployment's
// container args/command, or returns "" if not found.
func extractGCSBucketArg(d *appsv1.Deployment) string {
	if d == nil {
		return ""
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		for _, argv := range [][]string{c.Args, c.Command} {
			if bucket := findGCSBucketFlag(argv); bucket != "" {
				return normalizeBucketName(bucket)
			}
		}
	}
	return ""
}

func findGCSBucketFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		for _, prefix := range []string{"--gcs-bucket=", "-gcs-bucket="} {
			if strings.HasPrefix(arg, prefix) {
				return strings.TrimPrefix(arg, prefix)
			}
		}
		if (arg == "--gcs-bucket" || arg == "-gcs-bucket") && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
