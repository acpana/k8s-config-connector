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

package sql

import (
	"testing"

	api "google.golang.org/api/sqladmin/v1beta4"
)

func TestDiffInstances_StrictPointersMatch(t *testing.T) {
	// desired has all optional struct pointers as nil (typical for a minimal KRM spec)
	desired := &api.DatabaseInstance{
		Settings: &api.Settings{},
	}

	// actual has these fields populated with "empty" defaults by the GCP API
	actual := &api.DatabaseInstance{
		DiskEncryptionConfiguration: &api.DiskEncryptionConfiguration{
			Kind: "sql#diskEncryptionConfiguration",
		},
		ReplicaConfiguration: &api.ReplicaConfiguration{
			Kind: "sql#replicaConfiguration",
		},
		ReplicationCluster: &api.ReplicationCluster{},
		Settings: &api.Settings{
			BackupConfiguration: &api.BackupConfiguration{
				Kind: "sql#backupConfiguration",
			},
			DataCacheConfig: &api.DataCacheConfig{},
			IpConfiguration: &api.IpConfiguration{
				Ipv4Enabled: true,
				SslMode:     "ALLOW_UNENCRYPTED_AND_ENCRYPTED",
			},
			LocationPreference: &api.LocationPreference{
				Kind: "sql#locationPreference",
			},
		},
	}

	diff := DiffInstances(desired, actual)

	// Currently, this fails because PointersMatch is too strict.
	// We want HasDiff() to be false because these are semantically equivalent.
	if diff.HasDiff() {
		t.Errorf("DiffInstances() identified unexpected diffs: %v", diff.Fields)
	}
}
