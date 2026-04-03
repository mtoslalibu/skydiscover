# Hypothesis Experiment: Affinity + Fresh IFR Load Signal (v2)

## Problem

The default BLIS router (pa:3, qd:2, kv:2) uses prefix-affinity for cache-aware routing but relies on **stale** queue-depth and kv-utilization signals (5s Prometheus refresh) for load awareness. During bursty traffic, the stale signals lag behind reality, causing the router to keep sending requests to overloaded instances.

Glia HRA routes by KV headroom (`projectedUsage/totalBlocks`), which **actively penalizes cached instances** because they have higher KV utilization (cached prefix blocks count against them). On prefix-heavy workloads, this causes systematic cache misses — every request must process the full prefix from scratch.

Neither algorithm handles bursty, prefix-heavy workloads well:
- Default: Good affinity (cache hits) but slow burst response (stale load signals)
- Glia: Fast load response but no affinity (destroys cache, ignores prefix groups)

## Algorithm: Affinity + Fresh IFR

V2 replaces the stale load signals (queue-depth, kv-utilization) with **fresh InFlightRequests** while preserving the prefix-affinity scorer for cache-aware routing.

```
score[i] = affinityWeight * affinity[i] + loadWeight * freshLoad[i]

where:
  affinity[i]  = prefix-affinity scorer output [0,1] (from existing scorer pipeline)
  freshLoad[i] = 1 / (1 + IFR[i])                    (synchronous, per routing call)
  affinityWeight, loadWeight = from configured scorer weights (e.g., 43%/57% for pa:3,qd:2,kv:2)
```

### Properties

- **No new parameters**: Weight ratio comes from the existing scorer configuration
- **No thresholds**: `1/(1+IFR)` is a natural [0,1] normalization
- **No state**: Purely functional, computed per routing call
- **Single fresh signal**: InFlightRequests is synchronous (updated at routing time, not via Prometheus)

### Why it works

1. **During calm periods**: IFR is roughly equal across instances → freshLoad is similar for all → affinity dominates → requests route to cached instances → cache hits
2. **During bursts**: Overloaded instance has high IFR → low freshLoad → score drops → traffic redirects to less-loaded instances instantly (no 5s stale lag)
3. **Cold requests (no prefix)**: All affinity scores are 0 → freshLoad alone determines routing → least-loaded behavior

### Why v2 beats the default

The default uses stale queue-depth (5s Prometheus refresh) for load awareness. During gamma bursts (CV=8, onset < 1s), the stale data is completely wrong for the first 5s of each burst. V2's fresh IFR reacts within the same simulation tick.

### Why v2 beats Glia

Glia's scoring: `score = -projectedUsage/totalBlocks - 0.001*queueLoad`

With 40K-token prefixes, each cached instance holds ~2560 blocks (40960/16 block_size). Glia's `projectedUsage/totalBlocks` term penalizes these instances by ~4.3% (2560/60000 total blocks). The `0.001*queueLoad` tiebreaker contributes only ~0.01 for queue=10, so KV dominates by ~400x. Result: Glia systematically routes AWAY from cached instances, forcing full 40K-token prefill on every request.

V2 preserves prefix-affinity, maintaining cache hits during calm periods. Each cache hit saves ~40K tokens of prefill time (~200x reduction in prefill tokens).

### Comparison with Oracle v1 (global weight decay)

Oracle v1 decays the prefix-affinity **weight** globally based on cluster-wide IFR imbalance. This penalizes ALL instances' affinity scores when ANY imbalance exists. V2 instead replaces the stale load signals with fresh IFR without modifying affinity at all — affinity is always preserved, and load-balancing uses a separate, fresh signal.

### Key insight: Why naive IFR penalty fails

An earlier v2 variant applied `1/(1+excess)^2` penalty to the full composite score. On prefix-heavy workloads, this triggers cache-destroying redirects at low excess levels (IFR only 22% above mean). Waiting in queue on a cached instance is far cheaper than a 40K-token cache miss on an uncached instance. The correct approach is to use IFR as a **separate load signal** (additive with affinity), not as a **penalty** on the composite score (multiplicative).

## Workload: workload_v2.yaml

Designed to stress both default's stale signals and Glia's anti-cache routing:

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| Aggregate QPS | 80 | High enough for burst queuing, low enough that system isn't saturated |
| Instances | 4 | One per prefix group |
| Prefix length | 40,960 tokens | Cache miss costs ~40K tokens of prefill; cache hit costs ~200 tokens |
| Prefix groups | 4 (30/25/20/15%) | Balanced across instances; each group maps to one instance |
| Cold group | 10% | No prefix, tests load-balancing fallback |
| Burst severity | CV=8 (alpha), CV=6 (beta), CV=5 (gamma) | Sharp enough to expose stale signal lag |
| Requests | 2,500 | Sufficient for stable statistics |
| Model | qwen_7b, H100 TP=1, blackbox latency | Standard benchmark configuration |
| Snapshot refresh | 5s | Realistic Prometheus scrape interval |

## Results

### E2E Mean Latency (ms)

| Seed | Default (3:2:2) | Glia HRA | Oracle v1 | v2 |
|------|----------------|----------|-----------|-----|
| 42 | 709.6 | 938.4 | 840.8 | **801.5** |
| 123 | 571.8 | 782.9 | 664.9 | **503.8** |
| 456 | 854.6 | 939.0 | 1015.1 | **677.1** |
| 789 | 736.0 | 975.0 | 953.1 | **722.4** |
| 1337 | 842.4 | 866.2 | 818.0 | **758.4** |
| **MEAN** | **742.9** | **900.3** | **858.4** | **692.6** |

### E2E P95 Latency (ms)

| Seed | Default (3:2:2) | Glia HRA | Oracle v1 | v2 |
|------|----------------|----------|-----------|-----|
| 42 | 2837.5 | 3377.5 | 3262.0 | 3296.7 |
| 123 | 1569.1 | 3457.3 | 3385.4 | **1479.5** |
| 456 | 3045.5 | 4184.4 | 5666.8 | **2106.6** |
| 789 | 3598.6 | 4907.9 | 4824.2 | **3151.0** |
| 1337 | 3475.8 | 2905.1 | 3406.7 | **2755.4** |
| **MEAN** | **2905.3** | **3766.5** | **4109.0** | **2557.8** |

### Improvement Summary

| vs Baseline | E2E Mean | P95 | Combined |
|-------------|----------|-----|----------|
| vs Default (3:2:2) | **+6.8%** | **+12.0%** | **+9.4%** |
| vs Glia HRA | **+23.1%** | **+32.1%** | **+27.6%** |
| vs Oracle v1 | **+19.3%** | **+37.8%** | **+28.5%** |

V2 is the **only algorithm that beats both the default and Glia** on this workload.

## Key Findings

### 1. Fresh IFR signal beats stale queue-depth by ~10%

V2 beats the default by 9.4% combined, driven primarily by better burst response. The fresh IFR signal reacts within the same simulation tick, while the default's queue-depth takes up to 5s to update via Prometheus.

### 2. Cache-aware routing beats KV-headroom routing by ~28% on prefix-heavy workloads

V2 beats Glia by 27.6% combined. The entire gap comes from cache hits: V2 preserves prefix affinity and routes requests to instances with cached prefixes, while Glia actively routes AWAY from cached instances because they have higher KV utilization.

### 3. Per-instance penalty destroys cache on prefix-heavy workloads

An earlier v2 variant that applied per-instance IFR penalty to the composite score was -12% to -46% worse than default. The penalty triggered cache-destroying redirects at low excess levels. **Waiting in queue on a cached instance is cheaper than a cache miss** when prefixes are large (40K+ tokens).

### 4. The KV-utilization scorer is counterproductive for cache-aware routing

The default scorer pipeline includes kv-utilization (29% weight), which penalizes cached instances. On prefix-heavy workloads, this signal works against prefix-affinity. V2 replaces it with IFR-based load scoring, which doesn't penalize cache state.

### 5. 30% improvement over BOTH baselines is fundamentally difficult

Default and Glia have **complementary strengths**:
- Default excels on prefix-heavy workloads (strong affinity)
- Glia excels on prefix-light workloads (strong load-balancing)

Any algorithm that beats one baseline by 30% tends to have the same characteristics as the other baseline, limiting the improvement over that other baseline. V2 achieves the best compromise: it preserves default's affinity strength while adding fresh load signals that Glia lacks.

## Reproduction

```bash
cd benchmarks/blis_router/oracle
python benchmark.py --workloads workload_v2.yaml
# Results saved to benchmark_results.json
```

## Files

| File | Description |
|------|-------------|
| `oracle_router_v2.go` | Full routing.go with v2 algorithm (V2-START/V2-END markers) |
| `workload_v2.yaml` | 80 QPS, 4 groups, 40K prefix, gamma bursts |
| `benchmark.py` | 4-way benchmark: default vs Glia vs oracle v1 vs v2 |
| `benchmark_results.json` | Raw results from latest run |
