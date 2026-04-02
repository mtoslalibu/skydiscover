# Sim2Real Package Tracker

Quick reference for all sim2real packages — which experiment each came from, key results, and where to look.

---

## blis_router

| Field | Value |
|-------|-------|
| **Package** | `sim2real/blis_router/` |
| **Source experiment** | `outputs/blis_router/260312_50i_openevolve_v2wl/` |
| **Framework** | OpenEvolve (external backend) |
| **Iterations** | 50 (best at iteration 16) |
| **Seeds** | 42, 456 |
| **Models** | claude-sonnet-4-5 (70%) + claude-opus-4-5 (30%) |
| **Task** | Route requests across 4 qwen2.5-7b/H100 instances |
| **Workloads** | `glia_40qps` (40 QPS, bursty) + `prefix_heavy` (85 QPS, shared prefix groups) |
| **Scoring** | `0.5 × E2E_mean + 0.5 × E2E_P95`, normalized vs 1:1 baseline |
| **Baseline (1:1)** | glia_40qps: 4314 ms · prefix_heavy: 790 ms |
| **Evolved** | glia_40qps: 4303 ms · prefix_heavy: 700 ms · **+11.5% combined** |
| **inference-sim commit** | `7fd7a88d5d5005b15b142fa8e70cf5d8537ceebe` |

**What was discovered**: Adaptive KV-aware routing that blends prefix affinity with load balance, weighted by per-instance free KV blocks. Outperforms all hand-tuned baselines (LLQ, LOR, Glia, 1:1).

---

## admission_old

| Field | Value |
|-------|-------|
| **Package** | `sim2real/admission_old/` |
| **Source experiment** | `outputs/blis_admission/260312_50i_admission_openevolve/` |
| **Framework** | OpenEvolve (external backend) |
| **Iterations** | 50 (best at iteration 28) |
| **Seeds** | 42 (single seed) |
| **Models** | claude-sonnet-4-5 (70%) + claude-opus-4-6 (30%) |
| **Task** | Admit/reject requests on 4 qwen2.5-7b/H100 instances under 2× overload |
| **Workloads** | `overload_mixed_slo` + `bursty_adversary` (both at 320 QPS = 2× saturation) |
| **Scoring** | `0.50 × weighted_SLO + 0.30 × capped_throughput + 0.20 × Jain_fairness` |
| **Baseline score** | 0.624 (SLO attainment ~25%) |
| **Evolved score** | 0.852 (**+36.5%**) (SLO attainment ~87%) |
| **Prompt** | Had hardcoded hints (signal names + threshold values) — essentially gave away the answer |
| **Limitation** | Hardcoded thresholds (12, 20, 25, 35 req/instance) tuned to this simulation; may not generalize |

**What was discovered**: Load-based shedding with tenant fairness correction. Sheds `batch` at load > 12/instance and `sheddable` at load > 25/instance, with thresholds relaxed for tenants below 40% admit rate.

---

## admission_by60

| Field | Value |
|-------|-------|
| **Package** | `sim2real/admission_by60/` |
| **Source experiment** | `outputs/blis_admission/260324_30i_nohint_2seed/` |
| **Framework** | OpenEvolve (external backend) |
| **Iterations** | 30 (best at iteration 7) |
| **Seeds** | 42, 456 (two seeds — more robust than admission_old) |
| **Models** | claude-sonnet-4-5 (70%) + claude-opus-4-5 (30%) |
| **Task** | Same as admission_old — admit/reject under 2× overload |
| **Workloads** | Same workloads as admission_old (320 QPS, 2× saturation) |
| **Scoring** | Same formula as admission_old |
| **Baseline score** | 0.627 (SLO attainment ~25%) |
| **Evolved score** | 0.870 (**+38.8%**) (SLO attainment ~84%) |
| **Fairness** | 0.741 (vs 0.579 in admission_old — significantly better) |
| **Avg E2E** | 1,197 ms (vs 9,083 ms baseline — 6.8× faster) |
| **Prompt** | Redesigned: no hardcoded hints, added GENERALIZATION REQUIREMENT |
| **Key improvement** | Self-calibrating `typicalLoad` via 10s sliding window; no fixed constants |

**What was discovered**: Adaptive, normalized load shedding. Computes `loadRatio = perInstanceLoad / typicalLoad` where `typicalLoad` bootstraps at 40 req/s and self-calibrates from observed traffic. Sheds `batch` when `loadRatio > 0.50` and `sheddable` when `loadRatio > 0.75`, with thresholds tightened for tenants consuming >1.5× their fair share.

**Why better than admission_old**: No magic constants — policy adapts to actual cluster behavior and should generalize across different workload intensities and cluster sizes.

---

## Summary table

| Package | Experiment | Date | Iters | Seeds | Score | vs Baseline |
|---------|-----------|------|-------|-------|-------|-------------|
| `blis_router` | `260312_50i_openevolve_v2wl` | 2026-03-12 | 50 (best@16) | 42, 456 | — | +11.5% vs 1:1 |
| `admission_old` | `260312_50i_admission_openevolve` | 2026-03-12 | 50 (best@28) | 42 | 0.852 | +36.5% |
| `admission_by60` | `260324_30i_nohint_2seed` | 2026-03-24 | 30 (best@7) | 42, 456 | 0.870 | +38.8% |
