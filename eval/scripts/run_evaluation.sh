#!/usr/bin/env bash
set -euo pipefail

# Evaluation parameters (matching dissertation methodology)
MSG_RATES=(1 4 7 10 13 16 19)
REPETITIONS=10
RESULTS_FILE="eval/results/migration-metrics-$(date +%Y%m%d-%H%M%S).csv"
NAMESPACE="${NAMESPACE:-default}"
TARGET_NODE="${TARGET_NODE:-}"  # must be set
CHECKPOINT_REPO="${CHECKPOINT_REPO:-localhost:5000/checkpoints}"

mkdir -p "$(dirname "$RESULTS_FILE")"

# CSV header
echo "run,msg_rate,total_time_s,checkpoint_s,transfer_s,restore_s,replay_s,finalize_s,status" > "$RESULTS_FILE"

echo "=== MS2M Evaluation ==="
echo "Message rates: ${MSG_RATES[*]}"
echo "Repetitions: $REPETITIONS"
echo "Results: $RESULTS_FILE"
echo ""

run_counter=0

for rate in "${MSG_RATES[@]}"; do
    echo "--- Rate: $rate msg/s ---"

    # Update producer rate
    kubectl set env deployment/message-producer MSG_RATE="$rate" -n "$NAMESPACE"

    # Wait for producer to stabilize
    kubectl rollout status deployment/message-producer -n "$NAMESPACE" --timeout=60s
    sleep 10  # let queue reach steady state

    for rep in $(seq 1 $REPETITIONS); do
        run_counter=$((run_counter + 1))
        echo "  Run $run_counter (rate=$rate, rep=$rep/$REPETITIONS)"

        MIGRATION_NAME="eval-run-${run_counter}"

        # Create StatefulMigration CR
        cat <<YAML | kubectl apply -n "$NAMESPACE" -f -
apiVersion: migration.ms2m.io/v1alpha1
kind: StatefulMigration
metadata:
  name: ${MIGRATION_NAME}
spec:
  sourcePod: consumer-0
  targetNode: ${TARGET_NODE}
  checkpointImageRepository: ${CHECKPOINT_REPO}
  replayCutoffSeconds: 120
  messageQueueConfig:
    queueName: app.events
    brokerUrl: amqp://guest:guest@rabbitmq:5672/
    exchangeName: app.fanout
YAML

        # Wait for completion (timeout 10 minutes)
        echo -n "    Waiting..."
        for i in $(seq 1 120); do
            PHASE=$(kubectl get statefulmigration "$MIGRATION_NAME" -n "$NAMESPACE" \
                -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
            if [[ "$PHASE" == "Completed" || "$PHASE" == "Failed" ]]; then
                break
            fi
            sleep 5
            echo -n "."
        done
        echo " $PHASE"

        # Extract metrics
        TIMINGS=$(kubectl get statefulmigration "$MIGRATION_NAME" -n "$NAMESPACE" -o json)
        CHECKPOINT_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Checkpointing // "N/A"')
        TRANSFER_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Transferring // "N/A"')
        RESTORE_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Restoring // "N/A"')
        REPLAY_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Replaying // "N/A"')
        FINALIZE_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Finalizing // "N/A"')

        START_TIME=$(echo "$TIMINGS" | jq -r '.status.startTime // ""')
        if [[ -n "$START_TIME" && "$PHASE" == "Completed" ]]; then
            # Calculate total time from start to last condition update
            TOTAL_T=$(echo "$TIMINGS" | jq -r '
                (.status.startTime | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) as $start |
                (.status.conditions[-1].lastTransitionTime | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) as $end |
                ($end - $start) | tostring + "s"')
        else
            TOTAL_T="N/A"
        fi

        echo "$run_counter,$rate,$TOTAL_T,$CHECKPOINT_T,$TRANSFER_T,$RESTORE_T,$REPLAY_T,$FINALIZE_T,$PHASE" >> "$RESULTS_FILE"

        # Cleanup: delete the migration CR
        kubectl delete statefulmigration "$MIGRATION_NAME" -n "$NAMESPACE" --ignore-not-found

        # Wait for consumer to be re-created and ready
        sleep 15
    done
done

echo ""
echo "=== Evaluation Complete ==="
echo "Results saved to: $RESULTS_FILE"
echo "Total runs: $run_counter"
