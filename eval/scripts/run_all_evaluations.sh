#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# Master Evaluation Script
#
# Runs all 3 migration configurations sequentially:
#   1. statefulset-sequential  (baseline)
#   2. deployment-registry     (Deployment + ShadowPod + Registry transfer)
#   3. deployment-direct       (Deployment + ShadowPod + Direct transfer)
#
# Each configuration deploys its consumer workload, runs the evaluation,
# then cleans up before switching.
#
# Usage:
#   TARGET_NODE=worker-2 bash eval/scripts/run_all_evaluations.sh
#
# Environment:
#   TARGET_NODE       - Required. Target node for migration.
#   NAMESPACE         - Namespace for workloads (default: default)
#   MSG_RATES         - Space-separated rates (default: "1 4 7 10 13 16 19")
#   REPETITIONS       - Runs per rate (default: 10)
# =============================================================================

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
NAMESPACE="${NAMESPACE:-default}"
TARGET_NODE="${TARGET_NODE:-}"

if [[ -z "$TARGET_NODE" ]]; then
    echo "ERROR: TARGET_NODE must be set"
    exit 1
fi

CONSUMER_SS="$PROJECT_ROOT/eval/workloads/consumer.yaml"
CONSUMER_DEPLOY="$PROJECT_ROOT/eval/workloads/consumer-deployment.yaml"

CONFIGURATIONS=("statefulset-sequential" "deployment-registry" "deployment-direct")

echo "============================================="
echo "  MS2M Full Evaluation Suite"
echo "============================================="
echo "Target node: $TARGET_NODE"
echo "Namespace:   $NAMESPACE"
echo "Configs:     ${CONFIGURATIONS[*]}"
echo ""

EVAL_START=$(date +%s)

for config in "${CONFIGURATIONS[@]}"; do
    echo ""
    echo "============================================="
    echo "  Configuration: $config"
    echo "============================================="
    echo ""

    # Deploy the right consumer workload
    case "$config" in
        statefulset-sequential)
            echo "Deploying StatefulSet consumer..."
            kubectl apply -n "$NAMESPACE" -f "$CONSUMER_SS"
            echo "Waiting for consumer-0 to be ready..."
            sleep 15
            kubectl -n "$NAMESPACE" wait --for=condition=Ready pod/consumer-0 --timeout=120s
            ;;
        deployment-*)
            echo "Deploying Deployment consumer..."
            kubectl apply -n "$NAMESPACE" -f "$CONSUMER_DEPLOY"
            kubectl -n "$NAMESPACE" rollout status deployment/consumer --timeout=120s
            sleep 10
            ;;
    esac

    # Run the evaluation
    CONFIGURATION="$config" \
    NAMESPACE="$NAMESPACE" \
    TARGET_NODE="$TARGET_NODE" \
        bash "$SCRIPT_DIR/run_optimized_evaluation.sh"

    # Cleanup consumer workload before switching
    echo ""
    echo "Cleaning up $config consumer..."
    case "$config" in
        statefulset-sequential)
            kubectl delete -n "$NAMESPACE" -f "$CONSUMER_SS" --ignore-not-found --wait=true
            sleep 10
            ;;
        deployment-*)
            # Only clean up if next config is not also deployment-based
            NEXT_CONFIG=""
            found=false
            for c in "${CONFIGURATIONS[@]}"; do
                if [[ "$found" == "true" ]]; then
                    NEXT_CONFIG="$c"
                    break
                fi
                if [[ "$c" == "$config" ]]; then
                    found=true
                fi
            done

            if [[ -z "$NEXT_CONFIG" || "$NEXT_CONFIG" == statefulset-* ]]; then
                kubectl delete -n "$NAMESPACE" -f "$CONSUMER_DEPLOY" --ignore-not-found --wait=true
                sleep 10
            else
                echo "  Keeping Deployment consumer for next config."
            fi
            ;;
    esac
done

# Final cleanup
echo ""
echo "Final cleanup..."
kubectl delete -n "$NAMESPACE" -f "$CONSUMER_DEPLOY" --ignore-not-found 2>/dev/null || true
kubectl delete -n "$NAMESPACE" -f "$CONSUMER_SS" --ignore-not-found 2>/dev/null || true

ELAPSED=$(( $(date +%s) - EVAL_START ))
echo ""
echo "============================================="
echo "  Full Evaluation Complete (${ELAPSED}s)"
echo "============================================="
echo "Results in eval/results/"
ls -la "$PROJECT_ROOT/eval/results/"*.csv 2>/dev/null || echo "  (no results found locally)"
