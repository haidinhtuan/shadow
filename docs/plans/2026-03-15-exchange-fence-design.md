# Exchange-Fence Convergence — Design & Implementation Plan

## Overview

Add Exchange-Fence Convergence as a synchronization mechanism for the MiniReplay
during StatefulSet identity swap. Controlled via a new CRD field `identitySwapMode`.

## CRD Changes

### New Spec Field

```go
// IdentitySwapMode controls how StatefulSet identity is restored during Finalizing.
// "None" (default): no identity swap, shadow pod remains orphaned.
// "ExchangeFence": full identity swap with Exchange-Fence Convergence for
//   zero-gap state synchronization.
// "Cutoff": identity swap with time-based MiniReplay cutoff (15s).
IdentitySwapMode string `json:"identitySwapMode,omitempty"`
```

### SwapSubPhase Updates

Current sub-phases: PrepareSwap → ReCheckpoint → SwapTransfer → CreateReplacement → MiniReplay → TrafficSwitch

New sub-phases for ExchangeFence path:
PrepareSwap → ReCheckpoint → SwapTransfer → CreateReplacement → PreFenceDrain → ExchangeFence → ParallelDrain → FenceCutover

The Cutoff path keeps the existing sub-phases unchanged.

## Messaging Client Changes

### New Interface Methods

```go
// BindQueue binds a queue to an exchange with the given routing key.
BindQueue(ctx context.Context, queueName, exchangeName, routingKey string) error

// PurgeQueue removes all messages from a queue.
PurgeQueue(ctx context.Context, queueName string) error

// GetQueueStats returns messages_ready and messages_unacknowledged for a queue.
GetQueueStats(ctx context.Context, queueName string) (ready int, unacked int, err error)
```

## Controller Changes

### 1. handleFinalizing — Gate on IdentitySwapMode

```
if identitySwapMode == "None" || identitySwapMode == "":
    existing simple finalization (END_REPLAY, delete secondary, complete)
else:
    identity swap sub-phase loop (existing pattern)
```

### 2. handleSwapPrepare — Restructured

```
- Create swap queue + bind to exchange
- Start STS scale-down (async, non-blocking)
- Transition to ReCheckpoint immediately (don't wait for scale-down)
```

### 3. handleSwapReCheckpoint — Unchanged

CRIU re-checkpoint shadow pod. Swap queue was just created (~ms before).

### 4. handleSwapTransfer — Unchanged

Local load via ms2m-agent or Job fallback.

### 5. handleSwapCreateReplacement — Unchanged

Wait for original pod deleted, create replacement, wait Running.

### 6. handleSwapPreFenceDrain (NEW — ExchangeFence only)

```
- Send START_REPLAY(swap_queue) to replacement
- Poll swap queue depth every 2s
- Once depth stabilizes or is low enough:
    - Calculate estimated fence time vs 15s cutoff
    - If fence viable → transition to ExchangeFence
    - If not viable → transition to MiniReplay (Cutoff fallback)
```

### 7. handleSwapExchangeFence (NEW)

```
- Create buffer queue + bind to exchange
- Unbind primary queue from exchange
- Unbind swap queue from exchange
- Record fence timestamp + queue depths
- Transition to ParallelDrain
```

### 8. handleSwapParallelDrain (NEW)

```
- Poll both primary and swap queue depths
- Check: ready == 0 AND unacked == 0 for both queues
- Stall detection: if neither queue depth decreases for 6s → rollback
- Timeout ceiling: 1.5 × max(D_swap, D_primary) / R_out
- When both at 0 → transition to FenceCutover
```

### 9. handleSwapFenceCutover (NEW)

```
- Kill shadow pod (force-delete, 1s grace)
- Rebind primary queue to exchange
- Unbind buffer queue from exchange (seal it)
- Poll buffer queue: wait for ready == 0 AND unacked == 0
- Send END_REPLAY to replacement (switches to primary)
- Delete swap + buffer queues
- Scale up StatefulSet
- Mark complete
```

### 10. handleSwapRollback (NEW — called on any fence failure)

```
- Rebind primary queue to exchange (restore live service)
- Delete buffer queue (messages lost, but primary is receiving again)
- Delete swap queue
- Kill replacement pod
- Fail migration with descriptive reason
- On retry: new re-checkpoint is mandatory (shadow state has advanced)
```

## Implementation Tasks

### Task 1: CRD + Types
- Add `IdentitySwapMode` to StatefulMigrationSpec
- Update CRD YAML
- Update deepcopy if needed

### Task 2: Messaging Interface
- Add `BindQueue`, `PurgeQueue`, `GetQueueStats` to BrokerClient interface
- Implement in RabbitMQClient
- Implement in MockBrokerClient
- Add tests

### Task 3: Restructure PrepareSwap
- Start STS scale-down in PrepareSwap (async)
- Create swap queue as close to ReCheckpoint as possible

### Task 4: Gate Finalizing on IdentitySwapMode
- None → simple finalization
- ExchangeFence/Cutoff → identity swap loop
- Wire new sub-phases into the swap dispatcher

### Task 5: PreFenceDrain Sub-Phase
- Send START_REPLAY to replacement
- Poll swap queue, measure drain rate
- Adaptive decision: fence vs cutoff fallback

### Task 6: ExchangeFence + ParallelDrain Sub-Phases
- Bind buffer, unbind primary + swap
- Poll both queues with stall detection + timeout ceiling
- Rollback on failure

### Task 7: FenceCutover Sub-Phase
- Kill shadow, rebind primary, drain buffer, END_REPLAY
- Cleanup queues, scale up STS

### Task 8: Rollback Handler
- Rebind primary, clean up, fail migration

### Task 9: Tests
- Unit tests for all new sub-phases
- Test adaptive decision (fence vs cutoff)
- Test rollback on stall
- Test ExchangeFence happy path end-to-end
- Test Cutoff fallback path

### Task 10: Integration
- Run `make test` — all existing + new tests pass
- Manual verification of CRD changes
