# StatefulSet Post-Migration Ownership

## Problem

After migration, target/shadow pods are orphaned from their StatefulSet. This manifests differently per strategy:

**Sequential strategy**: Target pod has the correct name (`consumer-0`) but `ownerReferences` point to the `StatefulMigration` CR with `controller: true`. The StatefulSet controller cannot adopt this pod and cannot create a new one (name conflict).

**ShadowPod strategy**: Shadow pod (`consumer-0-shadow`) cannot be adopted due to its name (fails the StatefulSet `isMemberOf()` regex). The shadow pod serves traffic correctly but if it crashes, nobody restarts it.

## Current State

### Sequential: Fixed

In `handleFinalizing`, the controller removes the `StatefulMigration` ownerReference from the target pod before scaling the StatefulSet back up. The StatefulSet controller finds the orphan pod (matching name + labels) and adopts it automatically. Full StatefulSet guarantees (crash recovery, ordered scaling) are restored.

### ShadowPod + StatefulSet: Working, Orphaned

The ShadowPod strategy achieves zero downtime for StatefulSet migration (confirmed across 140 evaluation runs). After migration:
- The shadow pod serves traffic via the Service (carries the app labels)
- The StatefulSet is scaled down (source pod removed)
- The shadow pod is **not owned** by the StatefulSet

This means StatefulSet crash recovery and ordered scaling guarantees do not apply to the shadow pod. The migration itself is correct — zero downtime, zero message loss — but the post-migration lifecycle is degraded.

## Future Work: ShadowPod Re-Adoption

To fully restore StatefulSet ownership after ShadowPod migration, a re-adoption mechanism is needed. The general approach: re-checkpoint the shadow pod, create a correctly-named replacement pod (`consumer-0`), use a mini-replay round to synchronize state, then delete the shadow pod. The StatefulSet adopts the replacement as an orphan.

This reuses the existing migration machinery (CreateSecondaryQueue, SendControlMessage, GetQueueDepth) and would be implemented as sub-phases within Finalizing.
