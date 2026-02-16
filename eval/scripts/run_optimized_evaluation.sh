#!/usr/bin/env bash
set -euo pipefail

# Configuration: which optimized setup to evaluate
CONFIGURATION="${CONFIGURATION:-deployment-registry}"
# Options:
#   "statefulset-sequential" — baseline (uses consumer.yaml StatefulSet)
#   "deployment-registry"    — Deployment + ShadowPod + Registry transfer
#   "deployment-direct"      — Deployment + ShadowPod + Direct transfer

# Evaluation parameters (matching dissertation methodology)
MSG_RATES=(1 4 7 10 13 16 19)
REPETITIONS=10
RESULTS_FILE="eval/results/migration-metrics-${CONFIGURATION}-$(date +%Y%m%d-%H%M%S).csv"
NAMESPACE="${NAMESPACE:-default}"
TARGET_NODE="${TARGET_NODE:-}"  # must be set
CHECKPOINT_REPO="${CHECKPOINT_REPO:-registry.registry.svc.cluster.local:5000/checkpoints}"

if [[ -z "$TARGET_NODE" ]]; then
    echo "ERROR: TARGET_NODE must be set"
    exit 1
fi

mkdir -p "$(dirname "$RESULTS_FILE")"

# CSV header
echo "run,msg_rate,configuration,total_time_s,checkpoint_s,transfer_s,restore_s,replay_s,finalize_s,status" > "$RESULTS_FILE"

echo "=== MS2M Optimized Evaluation ==="
echo "Configuration: $CONFIGURATION"
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
        echo "  Run $run_counter (rate=$rate, rep=$rep/$REPETITIONS, config=$CONFIGURATION)"

        MIGRATION_NAME="eval-run-${run_counter}"

        # Determine source pod name
        if [[ "$CONFIGURATION" == statefulset-* ]]; then
            SOURCE_POD="consumer-0"
        else
            # Deployment pods have generated names; look up dynamically
            SOURCE_POD=$(kubectl get pods -n "$NAMESPACE" -l app=consumer -o jsonpath='{.items[0].metadata.name}')
            if [[ -z "$SOURCE_POD" ]]; then
                echo "    ERROR: No consumer pod found"
                echo "$run_counter,$rate,$CONFIGURATION,N/A,N/A,N/A,N/A,N/A,N/A,NoPod" >> "$RESULTS_FILE"
                continue
            fi
            echo "    Source pod: $SOURCE_POD"
        fi

        # Build optional CR fields based on configuration
        if [[ "$CONFIGURATION" == "deployment-direct" ]]; then
            TRANSFER_MODE_FIELD="  transferMode: Direct"
        else
            TRANSFER_MODE_FIELD=""
        fi

        if [[ "$CONFIGURATION" == statefulset-* ]]; then
            STRATEGY_FIELD=""
        else
            STRATEGY_FIELD="  migrationStrategy: ShadowPod"
        fi

        # Create StatefulMigration CR
        cat <<YAML | kubectl apply -n "$NAMESPACE" -f -
apiVersion: migration.ms2m.io/v1alpha1
kind: StatefulMigration
metadata:
  name: ${MIGRATION_NAME}
spec:
  sourcePod: ${SOURCE_POD}
  targetNode: ${TARGET_NODE}
  checkpointImageRepository: ${CHECKPOINT_REPO}
  replayCutoffSeconds: 120
${STRATEGY_FIELD}
${TRANSFER_MODE_FIELD}
  messageQueueConfig:
    queueName: app.events
    brokerUrl: amqp://guest:guest@rabbitmq.rabbitmq.svc.cluster.local:5672/
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

        echo "$run_counter,$rate,$CONFIGURATION,$TOTAL_T,$CHECKPOINT_T,$TRANSFER_T,$RESTORE_T,$REPLAY_T,$FINALIZE_T,$PHASE" >> "$RESULTS_FILE"

        # Cleanup: delete the migration CR
        kubectl delete statefulmigration "$MIGRATION_NAME" -n "$NAMESPACE" --ignore-not-found

        # Wait for consumer to be ready for next run
        if [[ "$CONFIGURATION" == statefulset-* ]]; then
            # StatefulSet: controller handles scale-down/up, wait for pod to return
            sleep 15
        else
            # Deployment: source pod is deleted during Finalizing, Deployment
            # controller recreates a new pod. Wait for it to be ready.
            kubectl rollout status deployment/consumer -n "$NAMESPACE" --timeout=120s
            sleep 10
        fi
    done
done

echo ""
echo "=== Evaluation Complete ==="
echo "Configuration: $CONFIGURATION"
echo "Results saved to: $RESULTS_FILE"
echo "Total runs: $run_counter"
