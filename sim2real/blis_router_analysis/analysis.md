# BLIS Router Sim2Real Analysis

AI-discovered routing algorithm (OpenEvolve, 50 iterations) vs hand-tuned 1:1 baseline,
evaluated on the `prefix_heavy` workload.

---

## Simulation results (seeds 42+456, 4 instances, refresh=5s)

| Program | E2E mean (ms) | E2E P95 (ms) | E2E improv | P95 improv |
|---------|--------------|-------------|-----------|-----------|
| 1:1 (default) | 790 | 1909 | — | — |
| Evolved (best) | 700 | 1435 | +11.4% | +24.8% |

Combined improvement (0.5×E2E + 0.5×P95, averaged across glia_40qps + prefix_heavy): **+11.5%**

---

## Parameter sweep: prefix_heavy workload

### Instances sweep (snapshot_refresh=5s fixed)

| Instances | 1:1 E2E | 1:1 P95 | Evol E2E | Evol P95 | E2E improv | P95 improv |
|-----------|---------|---------|----------|----------|-----------|-----------|
| 2 | 873 | 1962 | 805 | 1736 | +7.8% | +11.5% |
| 3 | 781 | 1768 | 741 | 1528 | +5.1% | +13.6% |
| **4** ★ | **790** | **1909** | **700** | **1435** | **+11.4%** | **+24.8%** |
| 6 | 695 | 1512 | 665 | 1357 | +4.2% | +10.3% |
| 8 | 661 | 1398 | 637 | 1277 | +3.7% | +8.7% |

4 instances is the sweet spot. With more instances load distributes naturally, reducing the
opportunity for smart routing to make a difference. With fewer, neither algorithm has room to maneuver.

### Snapshot refresh sweep (instances=4 fixed)

| Refresh | 1:1 E2E | 1:1 P95 | Evol E2E | Evol P95 | E2E improv | P95 improv |
|---------|---------|---------|----------|----------|-----------|-----------|
| 50 ms | 781 | 1887 | 707 | 1426 | +9.5% | +24.5% |
| 250 ms | 782 | 1886 | 707 | 1428 | +9.5% | +24.3% |
| 1 s | 782 | 1873 | 707 | 1423 | +9.6% | +24.0% |
| **5 s** ★ | **790** | **1909** | **700** | **1435** | **+11.4%** | **+24.8%** |
| 15 s | 774 | 1869 | 706 | 1442 | +8.7% | +22.9% |

Improvements are flat across all refresh intervals (~9-11% E2E, ~23-25% P95).
The evolved router's primary mechanism (`InFlightRequests`) is synchronous and router-local,
so gains are independent of Prometheus staleness. Good signal for real deployment.

---

## What the evolved program does

The EVOLVE-BLOCK modifies `WeightedScoring.Route()` with three additions over 1:1:

1. **Adaptive prefix-affinity decay**: finds the instance with the best prefix score; if it is
   more loaded than the cluster minimum (by `delta = cachedLoad - minInflight`), decays its
   prefix-affinity weight by `1 / (1 + 0.6 * delta)` and compensates the load-balance weight.
   Prevents hotspotting on cached instances.

2. **KV pressure penalty**: subtracts `0.5 * (KVUtil - 0.9) / 0.1` when any instance's KV
   utilization exceeds 90%. Avoids memory pressure before it triggers preemption.

3. **Fresh load tiebreaker**: adds `0.01 / (1 + InFlightRequests)` to break near-ties using
   the freshest available signal (synchronous, not 5s stale).

---

## Why gains are limited — structural analysis

### 1. Decay signal is incomplete

```go
cachedLoad = snap.InFlightRequests   // ignores QueueDepth and BatchSize
delta := cachedLoad - minInflight
```

`EffectiveLoad = QueueDepth + BatchSize + InFlightRequests`. An instance can have a deep queue
but low in-flight count (just started draining) and the decay under-reacts. Acting on
`InFlightRequests` alone misses a large component of actual instance load.

### 2. Only the single argmax prefix instance is considered

The code finds one `bestPrefixID` (the argmax of prefix-affinity scores) and adjusts the global
`aw[0]`/`aw[1]` weights based on that instance's load. With 6 prefix groups mapped to 4 instances,
there is often a *second* instance with a reasonable prefix match that is less loaded. The logic
never considers that fallback — when the top instance is busy, it collapses directly to pure
load-balance, discarding cache affinity entirely.

### 3. The decay is all-or-nothing

When `delta` is large, `aw[0]` (prefix weight) approaches 0 and `aw[1]` (load weight) approaches 1 —
effectively switching to pure load-balance and abandoning cache affinity. For group A (45% of
traffic, all sharing one 14336-token prefix), this removes cache hit savings right when the hot
instance needs them most. There is no middle ground such as routing overflow to a second-best
prefix instance rather than the globally least-loaded one.

### 4. KV penalty threshold activates too late

At 85 QPS with 14336-token prefixes, KV pressure builds well before 90%. By the time the penalty
fires, evictions or preemptions may already be underway. A lower threshold or a softer ramp
starting at ~70-80% would enable proactive avoidance rather than reactive damage control.

### 5. Tiebreaker magnitude is negligible

`0.01 / (1 + IFR)` maxes out at 0.01 while prefix-affinity scores span [0, 1]. For group A
(45% of traffic) there is nearly always a clear prefix winner — the tiebreaker only affects
cases where scores are already near-identical, which is rare.

### 6. No SLO-class awareness in routing

The workload has realtime (groups E+F, 15%), interactive (B+C+D, 40%), and batch (A, 45%).
Realtime requests are latency-sensitive and could tolerate a cache miss in exchange for routing
to a less-loaded instance. The evolved router treats all traffic identically regardless of
latency urgency — `RoutingSnapshot` does not even expose SLO class.

### 7. Reactive not proactive: no group-to-instance affinity

The fundamental ceiling: the workload has 6 prefix groups and 4 instances. An ideal policy would
consistently assign each group to a primary instance (consistent-hashing style) and only overflow
when that instance is saturated. The evolved router is reactive — it decays weights *after* a
hotspot has already formed — rather than proactively partitioning groups. This means it can only
recover after a load spike, not prevent it.

The workload YAML comment confirms the design intent:
> "increases prefix to 14336 tokens to widen Glia gap further. Also increases outputs slightly
> so the 1:1 hotspot still matters."

The evolved router is already near the ceiling of what is achievable by *reactively tuning scalar
weights* within the existing scorer framework.

---

## Directions to push further

To go meaningfully beyond ~11% E2E / ~25% P95 on this workload, the evolved program would need
mechanisms beyond weight adjustment:

- **Per-prefix-group consistent hashing with load-triggered overflow**: assign each group to a
  primary instance deterministically; only redirect to a secondary when the primary exceeds a
  load threshold. This prevents hotspots proactively.
- **Second-best prefix fallback**: instead of decaying to global least-loaded, find the instance
  with the second-highest prefix score and route there when the top instance is overloaded.
- **Earlier KV penalty ramp**: start penalizing at 70-80% KV utilization with a gradual ramp
  rather than a hard threshold at 90%.
- **SLO-class-aware routing**: expose SLO class to the routing decision; route realtime requests
  to least-loaded even at cache cost, batch requests to best-prefix even if slightly busier.
- **Full EffectiveLoad in decay**: replace `InFlightRequests` with `EffectiveLoad()` in the
  decay computation to account for queued and in-batch requests.
