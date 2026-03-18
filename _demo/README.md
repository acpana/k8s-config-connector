Script 0: 00_setup_gcp.sh

This script handles the initial GCP project setup, API enablement, GSA creation, and GCS bucket creation.

How to Run:
./00_setup_gcp.sh <YOUR_PROJECT_ID>
(Optional: [REGION] [BUCKET_SUFFIX] [KCC_GSA_NAME] [MCL_GSA_NAME])

Script 1: 01_create_clusters.sh

This script creates the two GKE clusters.

How to Run:
./01_create_clusters.sh <YOUR_PROJECT_ID>
(Optional: [REGION] [C1_NAME] [C2_NAME])

Script 2: 02_deploy_apps.sh

This script clones the required repositories, installs the KCC operator, CRDs, configures KCC in Namespaced Mode, deploys MCL and Syncer controllers, and sets up Workload Identity bindings and cross-cluster authentication.

How to Run:
./02_deploy_apps.sh <YOUR_PROJECT_ID> <BUCKET_NAME_FROM_SCRIPT_00>
(Optional: [REGION] [C1_NAME] [C2_NAME] [KCC_GSA_NAME] [MCL_GSA_NAME] [TENANT_NS] [LEASE_NAME] - must match values from previous scripts)

Script 3: 03_verify.sh

This script runs the operational verification and failover test.

How to Run:
./03_verify.sh <YOUR_PROJECT_ID> <BUCKET_NAME_FROM_SCRIPT_00>
(Optional: [REGION] [C1_NAME] [C2_NAME] [TENANT_NS] [LEASE_NAME] - must match values from previous scripts)

Script 4: 04_cleanup.sh

This script cleans up all the resources created by the previous scripts.

How to Run:
./04_cleanup.sh <YOUR_PROJECT_ID> <BUCKET_NAME_FROM_SCRIPT_00>
(Optional: [REGION] [C1_NAME] [C2_NAME] [KCC_GSA_NAME] [MCL_GSA_NAME] - must match values from previous scripts)

Sample Run:

```bash 
$ 00_setup_gcp.sh PROJECT_ID
...
--- GCP Setup Complete ---
Bucket Name: BUCKET_NAME

$ 01_create_clusters.sh PROJECT_ID
# takes ~ 10 minutes
...
Getting cluster credentials...                  
Fetching cluster endpoint and auth data.        
kubeconfig entry generated for kcc-ha-cluster-1.
Fetching cluster endpoint and auth data.        
kubeconfig entry generated for kcc-ha-cluster-2.

$ 02_deploy_apps.sh PROJECT_ID BUCKET_ID
# takes ~ 10 minutes
...
--- 5. Setup Cross-Cluster Authentication ---
secret/cluster-2 created                     
secret/cluster-1 created

$ 03_verify.sh PROJECT_ID BUCKET_ID
...
```
