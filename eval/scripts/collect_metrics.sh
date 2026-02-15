#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="${NAMESPACE:-default}"
OUTPUT="${1:-eval/results/collected-metrics.csv}"

mkdir -p "$(dirname "$OUTPUT")"

echo "name,phase,checkpoint_s,transfer_s,restore_s,replay_s,finalize_s,start_time" > "$OUTPUT"

kubectl get statefulmigrations -n "$NAMESPACE" -o json | jq -r '
  .items[] |
  [
    .metadata.name,
    .status.phase,
    (.status.phaseTimings.Checkpointing // "N/A"),
    (.status.phaseTimings.Transferring // "N/A"),
    (.status.phaseTimings.Restoring // "N/A"),
    (.status.phaseTimings.Replaying // "N/A"),
    (.status.phaseTimings.Finalizing // "N/A"),
    (.status.startTime // "N/A")
  ] | @csv' >> "$OUTPUT"

echo "Metrics collected to $OUTPUT"
echo "Entries: $(( $(wc -l < "$OUTPUT") - 1 ))"
