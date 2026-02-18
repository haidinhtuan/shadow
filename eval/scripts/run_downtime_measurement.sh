#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# Combined Migration Evaluation Script
#
# Measures both total migration time (with per-phase breakdown) and service
# downtime for each migration run. Downtime is measured by continuously
# probing consumer:8080 every 10ms during migration.
#
# Configurations:
#   1. statefulset-sequential  (baseline — source killed during Restore)
#   2. statefulset-shadowpod   (ShadowPod on StatefulSet — STS scaled to 0)
#   3. deployment-registry     (ShadowPod on Deployment — source stays until Finalizing)
#
# Default: 3 configs × 7 rates × 10 reps = 210 runs
#
# Usage:
#   bash eval/scripts/run_downtime_measurement.sh
#   REPETITIONS=1 bash eval/scripts/run_downtime_measurement.sh  # quick test
#
# Environment:
#   WORKER_1          - First worker node (default: worker-1)
#   WORKER_2          - Second worker node (default: worker-2)
#   NAMESPACE         - Namespace for workloads (default: default)
#   CHECKPOINT_REPO   - Checkpoint image repository
#   REPETITIONS       - Reps per rate/config (default: 10)
# =============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
NAMESPACE="${NAMESPACE:-default}"
WORKER_1="${WORKER_1:-worker-1}"
WORKER_2="${WORKER_2:-worker-2}"
CHECKPOINT_REPO="${CHECKPOINT_REPO:-registry.registry.svc.cluster.local:5000/checkpoints}"

# Worker IPs for checkpoint image cleanup (resolve from kubectl)
WORKER_1_IP=$(kubectl get node "$WORKER_1" -o jsonpath='{.status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null \
    || kubectl get node "$WORKER_1" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
WORKER_2_IP=$(kubectl get node "$WORKER_2" -o jsonpath='{.status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null \
    || kubectl get node "$WORKER_2" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')

CONSUMER_SS="$PROJECT_ROOT/eval/workloads/consumer.yaml"
CONSUMER_DEPLOY="$PROJECT_ROOT/eval/workloads/consumer-deployment.yaml"

CONFIGURATIONS=("statefulset-sequential" "statefulset-shadowpod" "deployment-registry")
# shellcheck disable=SC2206
MSG_RATES=(${MSG_RATES:-10 20 40 60 80 100 120})
REPETITIONS="${REPETITIONS:-10}"

RESULTS_FILE="eval/results/eval-metrics-$(date +%Y%m%d-%H%M%S).csv"
mkdir -p "$(dirname "$RESULTS_FILE")"

PROBE_LOG="/tmp/probe-log.txt"

# CSV header
echo "run,msg_rate,configuration,downtime_ms,down_probes,total_time_s,checkpoint_s,transfer_s,restore_s,replay_s,finalize_s,status" > "$RESULTS_FILE"

echo "============================================="
echo "  MS2M Combined Evaluation"
echo "============================================="
echo "Workers:     $WORKER_1, $WORKER_2"
echo "Namespace:   $NAMESPACE"
echo "Configs:     ${CONFIGURATIONS[*]}"
echo "Rates:       ${MSG_RATES[*]}"
echo "Reps:        $REPETITIONS"
echo "Results:     $RESULTS_FILE"
echo ""

# ---------------------------------------------------------------------------
# Probe pod setup
# ---------------------------------------------------------------------------
setup_probe() {
    echo "Setting up probe pod..."
    kubectl delete pod probe -n "$NAMESPACE" --ignore-not-found --wait=true 2>/dev/null || true
    kubectl run probe -n "$NAMESPACE" --image=python:3.11-slim --restart=Always -- sleep infinity
    kubectl wait --for=condition=Ready pod/probe -n "$NAMESPACE" --timeout=60s
    echo "Probe pod ready."
}

cleanup_probe() {
    kubectl delete pod probe -n "$NAMESPACE" --ignore-not-found 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# Probe: continuously probe consumer:8080 every 10ms, log timestamp + status
# ---------------------------------------------------------------------------
start_probe() {
    # Remove stale log
    kubectl exec probe -n "$NAMESPACE" -- sh -c "rm -f /tmp/probe.log" 2>/dev/null || true

    # Background probe loop inside the pod using Python for ms-precision timestamps.
    # 10ms interval (~100 probes/sec) balances resolution with ephemeral port safety.
    kubectl exec probe -n "$NAMESPACE" -- python3 -c '
import time, urllib.request
with open("/tmp/probe.log", "w") as f:
    while True:
        ts = int(time.time() * 1000)
        try:
            urllib.request.urlopen("http://consumer.'"$NAMESPACE"'.svc.cluster.local:8080", timeout=1)
            f.write(f"{ts} UP\n")
        except Exception:
            f.write(f"{ts} DOWN\n")
        f.flush()
        time.sleep(0.01)
' &
    PROBE_PID=$!
    echo "    Probe started (local PID $PROBE_PID)"
}

stop_probe() {
    # Kill the local kubectl exec process
    if [[ -n "${PROBE_PID:-}" ]]; then
        kill "$PROBE_PID" 2>/dev/null || true
        wait "$PROBE_PID" 2>/dev/null || true
        unset PROBE_PID
    fi
    # Kill the Python probe process inside the pod reliably
    kubectl exec probe -n "$NAMESPACE" -- pkill -f 'python3 -c' 2>/dev/null || true
    sleep 1
}

fetch_probe_log() {
    kubectl exec probe -n "$NAMESPACE" -- cat /tmp/probe.log > "$PROBE_LOG" 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# Calculate downtime from probe log within a time window
# Usage: calculate_downtime_ms <log_file> <window_start_ms> <window_end_ms>
# Only counts DOWN entries between window_start and window_end (inclusive).
# ---------------------------------------------------------------------------
calculate_downtime_ms() {
    local log_file="$1"
    local window_start="${2:-0}"
    local window_end="${3:-99999999999999}"

    if [[ ! -s "$log_file" ]]; then
        echo "0"
        return
    fi

    # Find the longest contiguous DOWN streak within the time window.
    # Two consecutive DOWNs are "contiguous" if separated by <=3s (accounts
    # for the 1s HTTP timeout that inflates the gap during DOWN periods).
    # This avoids false high downtime from scattered DOWNs (e.g. a random
    # blip + an endpoint update 20s later being counted as 20s downtime).
    awk -v ws="$window_start" -v we="$window_end" '
    $2 == "DOWN" && $1+0 >= ws+0 && $1+0 <= we+0 {
        ts = $1 + 0
        if (streak_start == 0) {
            streak_start = ts
            streak_end = ts
        } else if (ts - streak_end <= 3000) {
            streak_end = ts
        } else {
            dur = streak_end - streak_start
            if (dur > max_dur) max_dur = dur
            streak_start = ts
            streak_end = ts
        }
    }
    END {
        if (streak_start > 0) {
            dur = streak_end - streak_start
            if (dur > max_dur) max_dur = dur
        }
        # Add one probe cycle (~10ms) since last DOWN means service was
        # still down for at least one more interval.
        if (max_dur > 0 || streak_start > 0)
            printf "%d\n", max_dur + 10
        else
            print "0"
    }' "$log_file"
}

# ---------------------------------------------------------------------------
# Deploy consumer workload for a given configuration
# ---------------------------------------------------------------------------
deploy_consumer() {
    local config="$1"
    echo "  Deploying consumer for $config..."

    # Delete any existing consumer workload. Force-delete pods to avoid the
    # orphan adoption race: if a new StatefulSet is created while the old pod
    # is still gracefully terminating, the new STS adopts the dying pod.
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

# ---------------------------------------------------------------------------
# Main evaluation loop
# ---------------------------------------------------------------------------
EVAL_START=$(date +%s)
run_counter=0

# Deploy probe once
setup_probe

for config in "${CONFIGURATIONS[@]}"; do
    echo ""
    echo "============================================="
    echo "  Configuration: $config"
    echo "============================================="

    deploy_consumer "$config"

    for rate in "${MSG_RATES[@]}"; do
        echo ""
        echo "--- Rate: $rate msg/s ---"

        # Update producer rate
        kubectl set env deployment/message-producer MSG_RATE="$rate" -n "$NAMESPACE"
        kubectl rollout status deployment/message-producer -n "$NAMESPACE" --timeout=60s
        sleep 20  # let queue reach steady state

        # Find RabbitMQ pod
        RMQ_POD=$(kubectl get pods -n rabbitmq -o jsonpath='{.items[0].metadata.name}')

        for rep in $(seq 1 $REPETITIONS); do
            run_counter=$((run_counter + 1))
            echo "  Run $run_counter (rate=$rate, rep=$rep/$REPETITIONS, config=$config)"

            MIGRATION_NAME="dt-run-${run_counter}"

            # Determine source pod
            if [[ "$config" == statefulset-* ]]; then
                SOURCE_POD="consumer-0"
            else
                SOURCE_POD=$(kubectl get pods -n "$NAMESPACE" -l app=consumer -o jsonpath='{.items[0].metadata.name}')
                if [[ -z "$SOURCE_POD" ]]; then
                    echo "    ERROR: No consumer pod found"
                    echo "$run_counter,$rate,$config,0,N/A,N/A,N/A,N/A,N/A,N/A,NoPod" >> "$RESULTS_FILE"
                    continue
                fi
                echo "    Source pod: $SOURCE_POD"
            fi

            # Determine target node
            SOURCE_NODE=$(kubectl get pod "$SOURCE_POD" -n "$NAMESPACE" -o jsonpath='{.spec.nodeName}')
            if [[ "$SOURCE_NODE" == "$WORKER_1" ]]; then
                DYNAMIC_TARGET="$WORKER_2"
            else
                DYNAMIC_TARGET="$WORKER_1"
            fi
            echo "    Source: $SOURCE_POD on $SOURCE_NODE -> Target: $DYNAMIC_TARGET"

            # Build optional CR fields
            TRANSFER_MODE_FIELD=""

            if [[ "$config" == "statefulset-sequential" ]]; then
                STRATEGY_FIELD=""
            else
                STRATEGY_FIELD="  migrationStrategy: ShadowPod"
            fi

            # Purge queues
            kubectl exec -n rabbitmq "$RMQ_POD" -- rabbitmqctl purge_queue app.events 2>/dev/null || true
            kubectl exec -n rabbitmq "$RMQ_POD" -- rabbitmqctl delete_queue app.events.ms2m-replay 2>/dev/null || true
            sleep 3

            # Start probing
            start_probe

            # Give probe a few seconds of baseline readings
            sleep 2

            # Record migration start time (ms since epoch)
            MIGRATION_START_MS=$(python3 -c 'import time; print(int(time.time()*1000))')

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
${TRANSFER_MODE_FIELD}
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

            # Record migration end time (ms since epoch)
            MIGRATION_END_MS=$(python3 -c 'import time; print(int(time.time()*1000))')

            # Wait a bit after completion for endpoint updates to settle
            sleep 3

            # Stop probe and fetch log
            stop_probe
            fetch_probe_log

            # Calculate downtime only within the migration window
            DOWNTIME_MS=$(calculate_downtime_ms "$PROBE_LOG" "$MIGRATION_START_MS" "$MIGRATION_END_MS")
            DOWN_COUNT=$(awk -v ws="$MIGRATION_START_MS" -v we="$MIGRATION_END_MS" \
                '$2 == "DOWN" && $1+0 >= ws+0 && $1+0 <= we+0' "$PROBE_LOG" | wc -l)
            echo "    Downtime: ${DOWNTIME_MS}ms (${DOWN_COUNT} DOWN probes)"

            # Save probe log for analysis
            cp "$PROBE_LOG" "/tmp/probe-run-${run_counter}.log" 2>/dev/null || true

            # Extract phase timings
            TIMINGS=$(kubectl get statefulmigration "$MIGRATION_NAME" -n "$NAMESPACE" -o json)
            CHECKPOINT_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Checkpointing // "N/A"')
            TRANSFER_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Transferring // "N/A"')
            RESTORE_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Restoring // "N/A"')
            REPLAY_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Replaying // "N/A"')
            FINALIZE_T=$(echo "$TIMINGS" | jq -r '.status.phaseTimings.Finalizing // "N/A"')

            # Calculate total time (wall-clock from CR creation to completion detection)
            if [[ "$PHASE" == "Completed" ]]; then
                TOTAL_T_MS=$(( MIGRATION_END_MS - MIGRATION_START_MS ))
                TOTAL_T=$(awk "BEGIN {printf \"%.1fs\", $TOTAL_T_MS / 1000}")
            else
                TOTAL_T="N/A"
            fi

            echo "$run_counter,$rate,$config,$DOWNTIME_MS,$DOWN_COUNT,$TOTAL_T,$CHECKPOINT_T,$TRANSFER_T,$RESTORE_T,$REPLAY_T,$FINALIZE_T,$PHASE" >> "$RESULTS_FILE"

            # Cleanup migration CR and transfer artifacts
            kubectl delete statefulmigration "$MIGRATION_NAME" -n "$NAMESPACE" --ignore-not-found
            kubectl delete job -n "$NAMESPACE" -l "migration.ms2m.io/migration=$MIGRATION_NAME" --ignore-not-found 2>/dev/null || true

            # Clean up checkpoint images on workers to prevent disk exhaustion
            for worker_ip in $WORKER_1_IP $WORKER_2_IP; do
                ssh -o StrictHostKeyChecking=no "root@$worker_ip" \
                    'crictl images | grep "registry.registry.svc.cluster.local:5000/checkpoints" | awk "{print \$3}" | xargs -r crictl rmi 2>/dev/null; rm -f /var/lib/kubelet/checkpoints/*.tar' \
                    2>/dev/null &
            done
            wait

            # Wait for consumer to be ready for next run
            if [[ "$config" == "statefulset-shadowpod" ]]; then
                # ShadowPod + StatefulSet: finalization scaled STS to 0, scale back up
                echo -n "    Scaling StatefulSet back to 1..."
                kubectl scale statefulset consumer --replicas=1 -n "$NAMESPACE"
                for i in $(seq 1 60); do
                    if kubectl get pod consumer-0 -n "$NAMESPACE" &>/dev/null; then break; fi
                    sleep 2
                    echo -n "."
                done
                kubectl wait --for=condition=Ready pod/consumer-0 -n "$NAMESPACE" --timeout=120s 2>/dev/null || true
                sleep 5
                echo " ready"
            elif [[ "$config" == statefulset-* ]]; then
                # Sequential: StatefulSet scales back up automatically in finalization
                echo -n "    Waiting for consumer-0 readiness..."
                for i in $(seq 1 60); do
                    POD_STATUS=$(kubectl get pod consumer-0 -n "$NAMESPACE" -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || true)
                    if [[ -z "$POD_STATUS" ]]; then break; fi
                    sleep 2
                    echo -n "."
                done
                for i in $(seq 1 30); do
                    if kubectl get pod consumer-0 -n "$NAMESPACE" &>/dev/null; then break; fi
                    sleep 2
                    echo -n "+"
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
                    sleep 2
                    echo -n "."
                done
                echo ""
                kubectl rollout status deployment/consumer -n "$NAMESPACE" --timeout=120s || true
                sleep 5
                READY_PODS=$(kubectl get pods -n "$NAMESPACE" -l app=consumer \
                    --field-selector=status.phase=Running -o name 2>/dev/null | wc -l)
                echo "    Consumer pods ready: $READY_PODS"
            fi
        done
    done

    # Cleanup consumer workload before switching configs
    echo ""
    echo "  Cleaning up $config consumer..."
    case "$config" in
        statefulset-*)
            kubectl delete -n "$NAMESPACE" -f "$CONSUMER_SS" --ignore-not-found --wait=true
            sleep 10
            ;;
        deployment-*)
            kubectl delete -n "$NAMESPACE" -f "$CONSUMER_DEPLOY" --ignore-not-found --wait=true
            sleep 10
            ;;
    esac

    # Clean registry storage between configs to prevent disk exhaustion
    echo "  Cleaning registry storage..."
    REGISTRY_POD=$(kubectl get pods -n registry -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [[ -n "$REGISTRY_POD" ]]; then
        kubectl exec -n registry "$REGISTRY_POD" -- sh -c 'rm -rf /var/lib/registry/docker' 2>/dev/null || true
    fi
done

# Final cleanup
cleanup_probe
kubectl delete -n "$NAMESPACE" -f "$CONSUMER_DEPLOY" --ignore-not-found 2>/dev/null || true
kubectl delete -n "$NAMESPACE" -f "$CONSUMER_SS" --ignore-not-found 2>/dev/null || true

ELAPSED=$(( $(date +%s) - EVAL_START ))
echo ""
echo "============================================="
echo "  Combined Evaluation Complete (${ELAPSED}s)"
echo "============================================="
echo "Results: $RESULTS_FILE"
echo "Total runs: $run_counter"
