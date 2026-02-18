#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# Full Migration Time Evaluation
#
# Measures total migration time and phase durations for all configurations.
# Includes per-run checkpoint cleanup to prevent disk exhaustion.
#
# Configurations:
#   1. statefulset-sequential  (baseline)
#   2. statefulset-shadowpod   (ShadowPod on StatefulSet)
#   3. deployment-registry     (ShadowPod on Deployment)
# =============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
NAMESPACE="${NAMESPACE:-default}"
WORKER_1="${WORKER_1:-worker-1}"
WORKER_2="${WORKER_2:-worker-2}"
CHECKPOINT_REPO="${CHECKPOINT_REPO:-registry.registry.svc.cluster.local:5000/checkpoints}"

CONSUMER_SS="$PROJECT_ROOT/eval/workloads/consumer.yaml"
CONSUMER_DEPLOY="$PROJECT_ROOT/eval/workloads/consumer-deployment.yaml"

CONFIGURATIONS="${CONFIGURATIONS:-statefulset-sequential statefulset-shadowpod deployment-registry}"
MSG_RATES=(${MSG_RATES:-10 20 40 60 80 100 120})
REPETITIONS="${REPETITIONS:-10}"

RESULTS_DIR="eval/results"
mkdir -p "$RESULTS_DIR"
RESULTS_FILE="$RESULTS_DIR/migration-metrics-all-$(date +%Y%m%d-%H%M%S).csv"

# Resolve worker IPs for SSH cleanup
WORKER_1_IP=$(kubectl get node "$WORKER_1" -o jsonpath='{.status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null \
    || kubectl get node "$WORKER_1" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
WORKER_2_IP=$(kubectl get node "$WORKER_2" -o jsonpath='{.status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null \
    || kubectl get node "$WORKER_2" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')

echo "run,msg_rate,configuration,total_time_s,checkpoint_s,transfer_s,restore_s,replay_s,finalize_s,status" > "$RESULTS_FILE"

echo "============================================="
echo "  MS2M Full Evaluation"
echo "============================================="
echo "Workers:     $WORKER_1 ($WORKER_1_IP), $WORKER_2 ($WORKER_2_IP)"
echo "Configs:     $CONFIGURATIONS"
echo "Rates:       ${MSG_RATES[*]}"
echo "Reps:        $REPETITIONS"
echo "Results:     $RESULTS_FILE"
echo ""

# ---------------------------------------------------------------------------
deploy_consumer() {
    local config="$1"
    echo "  Deploying consumer for $config..."

    kubectl delete -n "$NAMESPACE" -f "$CONSUMER_SS" --ignore-not-found 2>/dev/null || true
    kubectl delete -n "$NAMESPACE" -f "$CONSUMER_DEPLOY" --ignore-not-found 2>/dev/null || true
    kubectl delete pod -l app=consumer -n "$NAMESPACE" --ignore-not-found --grace-period=0 --force 2>/dev/null || true
    echo "  Waiting for all consumer pods to terminate..."
    while kubectl get pods -n "$NAMESPACE" -l app=consumer -o name 2>/dev/null | grep -q .; do
        sleep 2
    done
    sleep 3

    case "$config" in
        statefulset-*)
            kubectl apply -n "$NAMESPACE" -f "$CONSUMER_SS"
            sleep 15
            kubectl -n "$NAMESPACE" wait --for=condition=Ready pod/consumer-0 --timeout=120s
            ;;
        deployment-*)
            kubectl apply -n "$NAMESPACE" -f "$CONSUMER_DEPLOY"
            kubectl -n "$NAMESPACE" rollout status deployment/consumer --timeout=120s
            sleep 10
            ;;
    esac
    echo "  Consumer ready."
}

cleanup_checkpoint_images() {
    for worker_ip in $WORKER_1_IP $WORKER_2_IP; do
        ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 "root@$worker_ip" \
            'crictl images 2>/dev/null | grep "registry.registry.svc.cluster.local:5000/checkpoints" | awk "{print \$3}" | xargs -r crictl rmi 2>/dev/null; rm -f /var/lib/kubelet/checkpoints/*.tar' \
            2>/dev/null &
    done
    wait
}

# ---------------------------------------------------------------------------
EVAL_START=$(date +%s)
run_counter=0

for config in $CONFIGURATIONS; do
    echo ""
    echo "============================================="
    echo "  Configuration: $config"
    echo "============================================="

    deploy_consumer "$config"

    for rate in "${MSG_RATES[@]}"; do
        echo ""
        echo "--- Rate: $rate msg/s ---"

        kubectl set env deployment/message-producer MSG_RATE="$rate" -n "$NAMESPACE"
        kubectl rollout status deployment/message-producer -n "$NAMESPACE" --timeout=60s
        sleep 15

        RMQ_POD=$(kubectl get pods -n rabbitmq -o jsonpath='{.items[0].metadata.name}')

        for rep in $(seq 1 $REPETITIONS); do
            run_counter=$((run_counter + 1))
            echo "  Run $run_counter (rate=$rate, rep=$rep/$REPETITIONS, config=$config)"

            MIGRATION_NAME="eval-run-${run_counter}"

            # Determine source pod
            if [[ "$config" == statefulset-* ]]; then
                SOURCE_POD="consumer-0"
            else
                SOURCE_POD=$(kubectl get pods -n "$NAMESPACE" -l app=consumer -o jsonpath='{.items[0].metadata.name}')
                echo "    Source pod: $SOURCE_POD"
            fi

            # Determine target node
            SOURCE_NODE=$(kubectl get pod "$SOURCE_POD" -n "$NAMESPACE" -o jsonpath='{.spec.nodeName}')
            if [[ "$SOURCE_NODE" == "$WORKER_1" ]]; then
                DYNAMIC_TARGET="$WORKER_2"
            else
                DYNAMIC_TARGET="$WORKER_1"
            fi
            echo "    $SOURCE_POD on $SOURCE_NODE -> $DYNAMIC_TARGET"

            # Strategy field
            if [[ "$config" == "statefulset-sequential" ]]; then
                STRATEGY_FIELD=""
            else
                STRATEGY_FIELD="  migrationStrategy: ShadowPod"
            fi

            # Purge queues
            kubectl exec -n rabbitmq "$RMQ_POD" -- rabbitmqctl purge_queue app.events 2>/dev/null || true
            kubectl exec -n rabbitmq "$RMQ_POD" -- rabbitmqctl delete_queue app.events.ms2m-replay 2>/dev/null || true
            sleep 3

            # Record wall-clock start
            START_MS=$(python3 -c 'import time; print(int(time.time()*1000))')

            # Create migration CR
            cat <<YAML | kubectl apply -n "$NAMESPACE" -f -
apiVersion: migration.ms2m.io/v1alpha1
kind: StatefulMigration
metadata:
  name: ${MIGRATION_NAME}
spec:
  sourcePod: ${SOURCE_POD}
  targetNode: ${DYNAMIC_TARGET}
  checkpointImageRepository: ${CHECKPOINT_REPO}
  replayCutoffSeconds: 120
${STRATEGY_FIELD}
  messageQueueConfig:
    queueName: app.events
    brokerUrl: amqp://guest:guest@rabbitmq.rabbitmq.svc.cluster.local:5672/
    exchangeName: app.fanout
YAML

            # Wait for completion (timeout 10 minutes, poll every 2s)
            echo -n "    Waiting..."
            for i in $(seq 1 300); do
                PHASE=$(kubectl get statefulmigration "$MIGRATION_NAME" -n "$NAMESPACE" \
                    -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
                if [[ "$PHASE" == "Completed" || "$PHASE" == "Failed" ]]; then
                    break
                fi
                sleep 2
                echo -n "."
            done
            echo " $PHASE"

            END_MS=$(python3 -c 'import time; print(int(time.time()*1000))')

            # Extract phase timings
            TIMINGS=$(kubectl get statefulmigration "$MIGRATION_NAME" -n "$NAMESPACE" -o json)
            CHECKPOINT_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Checkpointing // "N/A"')
            TRANSFER_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Transferring // "N/A"')
            RESTORE_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Restoring // "N/A"')
            REPLAY_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Replaying // "N/A"')
            FINALIZE_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Finalizing // "N/A"')

            # Wall-clock total
            if [[ "$PHASE" == "Completed" ]]; then
                TOTAL_T_MS=$(( END_MS - START_MS ))
                TOTAL_T=$(awk "BEGIN {printf \"%.1fs\", $TOTAL_T_MS / 1000}")
            else
                TOTAL_T="N/A"
            fi

            echo "$run_counter,$rate,$config,$TOTAL_T,$CHECKPOINT_T,$TRANSFER_T,$RESTORE_T,$REPLAY_T,$FINALIZE_T,$PHASE" >> "$RESULTS_FILE"

            # Cleanup
            kubectl delete statefulmigration "$MIGRATION_NAME" -n "$NAMESPACE" --ignore-not-found
            kubectl delete job -n "$NAMESPACE" -l "migration.ms2m.io/migration=$MIGRATION_NAME" --ignore-not-found 2>/dev/null || true
            cleanup_checkpoint_images

            # Wait for consumer to be ready for next run
            if [[ "$config" == "statefulset-shadowpod" ]]; then
                echo -n "    Scaling StatefulSet back..."
                kubectl scale statefulset consumer --replicas=1 -n "$NAMESPACE"
                for i in $(seq 1 60); do
                    if kubectl get pod consumer-0 -n "$NAMESPACE" &>/dev/null; then break; fi
                    sleep 2; echo -n "."
                done
                kubectl wait --for=condition=Ready pod/consumer-0 -n "$NAMESPACE" --timeout=120s 2>/dev/null || true
                sleep 5
                echo " ready"
            elif [[ "$config" == statefulset-* ]]; then
                echo -n "    Waiting for consumer-0..."
                for i in $(seq 1 60); do
                    POD_STATUS=$(kubectl get pod consumer-0 -n "$NAMESPACE" -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || true)
                    if [[ -z "$POD_STATUS" ]]; then break; fi
                    sleep 2; echo -n "."
                done
                for i in $(seq 1 30); do
                    if kubectl get pod consumer-0 -n "$NAMESPACE" &>/dev/null; then break; fi
                    sleep 2; echo -n "+"
                done
                kubectl wait --for=condition=Ready pod/consumer-0 -n "$NAMESPACE" --timeout=120s 2>/dev/null || true
                sleep 5
                echo " ready"
            else
                echo -n "    Waiting for cleanup..."
                for i in $(seq 1 60); do
                    TERMINATING=$(kubectl get pods -n "$NAMESPACE" -l app=consumer \
                        --field-selector=status.phase!=Running -o name 2>/dev/null | wc -l)
                    SHADOW=$(kubectl get pod -n "$NAMESPACE" -l "migration.ms2m.io/role=target" \
                        -o name 2>/dev/null | wc -l)
                    if [[ "$TERMINATING" -eq 0 && "$SHADOW" -eq 0 ]]; then break; fi
                    sleep 2; echo -n "."
                done
                echo ""
                kubectl rollout status deployment/consumer -n "$NAMESPACE" --timeout=120s || true
                sleep 5
                echo "    Consumer ready"
            fi
        done
    done

    # Cleanup between configs
    echo "  Cleaning up $config..."
    case "$config" in
        statefulset-*) kubectl delete -n "$NAMESPACE" -f "$CONSUMER_SS" --ignore-not-found --wait=true ;;
        deployment-*) kubectl delete -n "$NAMESPACE" -f "$CONSUMER_DEPLOY" --ignore-not-found --wait=true ;;
    esac
    sleep 10

    echo "  Cleaning registry storage..."
    REGISTRY_POD=$(kubectl get pods -n registry -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [[ -n "$REGISTRY_POD" ]]; then
        kubectl exec -n registry "$REGISTRY_POD" -- sh -c 'rm -rf /var/lib/registry/docker' 2>/dev/null || true
    fi
done

ELAPSED=$(( $(date +%s) - EVAL_START ))
echo ""
echo "============================================="
echo "  Full Evaluation Complete (${ELAPSED}s)"
echo "============================================="
echo "Results: $RESULTS_FILE"
echo "Total runs: $run_counter"
