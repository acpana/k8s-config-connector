#!/bin/bash
# Description: Creates two GKE clusters with Workload Identity enabled.

# Stop on any error
set -e

# --- Parameters ---
PROJECT_ID=${1:?Usage: $0 PROJECT_ID [REGION] [C1_NAME] [C2_NAME]}
REGION=${2:-us-central1}
C1_NAME=${3:-kcc-ha-cluster-1}
C2_NAME=${4:-kcc-ha-cluster-2}

# --- Derived Variables ---
ZONE="${REGION}-c"

echo "--- Configuration ---"
echo "PROJECT_ID: ${PROJECT_ID}"
echo "REGION: ${REGION}"
echo "ZONE: ${ZONE}"
echo "C1_NAME: ${C1_NAME}"
echo "C2_NAME: ${C2_NAME}"
echo "---------------------"

gcloud config set project ${PROJECT_ID}

echo "--- 0.2. Creating GKE Clusters ---"
if ! gcloud container clusters describe ${C1_NAME} --zone ${ZONE} --project=${PROJECT_ID} &> /dev/null; then
  echo "Creating Cluster 1: ${C1_NAME}..."
  gcloud container clusters create ${C1_NAME} --zone ${ZONE} --workload-pool="${PROJECT_ID}.svc.id.goog" --num-nodes=1 --machine-type=e2-standard-4 --project=${PROJECT_ID}
else
  echo "Cluster ${C1_NAME} already exists."
fi

if ! gcloud container clusters describe ${C2_NAME} --zone ${ZONE} --project=${PROJECT_ID} &> /dev/null; then
  echo "Creating Cluster 2: ${C2_NAME}..."
  gcloud container clusters create ${C2_NAME} --zone ${ZONE} --workload-pool="${PROJECT_ID}.svc.id.goog" --num-nodes=1 --machine-type=e2-standard-4 --project=${PROJECT_ID}
else
  echo "Cluster ${C2_NAME} already exists."
fi

echo "Getting cluster credentials..."
gcloud container clusters get-credentials ${C1_NAME} --zone ${ZONE} --project=${PROJECT_ID}
gcloud container clusters get-credentials ${C2_NAME} --zone ${ZONE} --project=${PROJECT_ID}

echo "--- Cluster Creation Complete ---"
