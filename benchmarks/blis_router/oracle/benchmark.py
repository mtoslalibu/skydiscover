#!/usr/bin/env python3
"""
Benchmark: Default vs Glia vs Oracle v1 vs v2 BLIS routers.

Runs all four routers against workload YAMLs and reports E2E mean/P95
latency with improvement percentages across multiple seeds.

Algorithms:
  - Default: Static weighted scoring (prefix-affinity:3, queue-depth:2, kv-utilization:2)
  - Glia HRA: KV-cache headroom allocator (projects block usage, scores by remaining headroom)
  - Oracle v1: Adaptive affinity decay (decays prefix-affinity weight based on IFR imbalance)
  - v2: Per-instance IFR penalty (penalizes overloaded instances, preserves idle affinity)

Usage:
    python benchmark.py                          # run with defaults (5 seeds, all workloads)
    python benchmark.py --seeds 42,123,456       # custom seeds
    python benchmark.py --workloads workload_v2.yaml  # single workload
    python benchmark.py --instances 4            # custom instance count
"""

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

SCRIPT_DIR = Path(__file__).resolve().parent
BLIS_ROUTER_DIR = SCRIPT_DIR.parent
SIM_DIR = BLIS_ROUTER_DIR / "inference-sim"
ROUTING_GO = SIM_DIR / "sim" / "routing.go"
ORACLE_GO = SCRIPT_DIR / "oracle_router.go"
ORACLE_V2_GO = SCRIPT_DIR / "oracle_router_v2.go"
GLIA_GO = SCRIPT_DIR / "baseline_glia.go"
# Use the local oracle copy of the workload (workload_v1.yaml in this directory).
WORKLOADS_DIR = SCRIPT_DIR

DEFAULT_SEEDS = [42, 123, 456, 789, 1337]
DEFAULT_INSTANCES = 4
DEFAULT_SNAPSHOT_REFRESH = 5_000_000  # 5s in microseconds
# Use precise-prefix-cache (queries actual KV cache state) instead of prefix-affinity
# (heuristic based on routing history). This is the production-accurate scorer.
DEFAULT_ROUTING_SCORERS = "precise-prefix-cache:3,queue-depth:2,kv-utilization:2"

# Markers in routing.go that delimit the replaceable block
BLOCK_START = "// Compute composite scores from all scorers"
BLOCK_END_MARKER = "bestIdx = tied[ws.rng.Intn(len(tied))]"
# Skip one extra line after the marker (the closing "}" of the if block)
BLOCK_END_SKIP_LINES = 1


def find_workloads(workload_filter: list[str] | None) -> list[Path]:
    """Find all workload YAML files, optionally filtered."""
    all_yamls = sorted(WORKLOADS_DIR.glob("workload_*.yaml"))
    if workload_filter:
        filtered = []
        for name in workload_filter:
            p = WORKLOADS_DIR / name
            if not p.exists():
                p = WORKLOADS_DIR / f"workload_{name}.yaml"
            if p.exists():
                filtered.append(p)
            else:
                print(f"WARNING: workload not found: {name}", file=sys.stderr)
        return filtered
    return all_yamls


def build_blis() -> Path:
    """Build the blis binary, return path."""
    binary = SIM_DIR / "blis"
    result = subprocess.run(
        ["go", "build", "-o", str(binary), "main.go"],
        cwd=SIM_DIR, check=False, capture_output=True, text=True, timeout=60,
    )
    if result.returncode != 0:
        raise RuntimeError(f"Go build failed:\n{result.stderr}")
    return binary


def run_blis(
    binary: Path, workload: Path, seed: int, instances: int,
    routing_policy: str = "weighted",
    routing_scorers: str | None = None,
) -> dict | None:
    """Run blis and return cluster metrics dict, or None on failure."""
    cmd = [
        str(binary), "run",
        "--model", "qwen/qwen2.5-7b-instruct",
        "--hardware", "H100", "--tp", "1",
        "--latency-model", "blackbox",
        "--num-instances", str(instances),
        "--routing-policy", routing_policy,
        "--workload-spec", str(workload),
        "--snapshot-refresh-interval", str(DEFAULT_SNAPSHOT_REFRESH),
        "--seed", str(seed),
        "--log", "warn",
    ]
    if routing_scorers:
        cmd.extend(["--routing-scorers", routing_scorers])

    try:
        result = subprocess.run(
            cmd, cwd=SIM_DIR, capture_output=True, text=True, timeout=300,
        )
    except subprocess.TimeoutExpired:
        print(f"  TIMEOUT: seed={seed}", file=sys.stderr)
        return None

    output = result.stdout + (result.stderr or "")
    return parse_cluster_metrics(output)


def parse_cluster_metrics(output: str) -> dict | None:
    """Extract cluster-level metrics from blis JSON output."""
    for match in re.finditer(r'\{[^{}]+\}', output):
        try:
            obj = json.loads(match.group())
            if obj.get("instance_id") == "cluster":
                return obj
        except json.JSONDecodeError:
            continue
    return None


def _extract_block(source_file: Path, start_marker: str, end_marker: str) -> str:
    """Extract code between start_marker and end_marker lines from a Go source file."""
    code = source_file.read_text()
    si = code.find(start_marker)
    ei = code.find(end_marker)
    if si == -1 or ei == -1:
        raise RuntimeError(f"Could not find {start_marker}/{end_marker} in {source_file.name}")
    # Include from start-of-line containing start_marker through end-of-line containing end_marker
    line_start = code.rfind('\n', 0, si) + 1
    line_end = code.index('\n', ei) + 1
    return code[line_start:line_end].rstrip()


def patch_routing_go(algo_block: str) -> str:
    """Replace the scoring block in routing.go with the given algorithm code.

    Returns the original routing.go content for restoration.
    """
    original = ROUTING_GO.read_text()

    start_idx = original.find(BLOCK_START)
    end_idx = original.find(BLOCK_END_MARKER)
    if start_idx == -1 or end_idx == -1:
        raise RuntimeError("Could not find scoring block markers in routing.go")

    # Include the end marker line + closing brace line after it
    end_idx = original.index('\n', end_idx) + 1
    for _ in range(BLOCK_END_SKIP_LINES):
        end_idx = original.index('\n', end_idx) + 1

    # Find the start of the line containing BLOCK_START
    line_start = original.rfind('\n', 0, start_idx) + 1

    patched = original[:line_start] + algo_block + '\n' + original[end_idx:]
    ROUTING_GO.write_text(patched)
    return original


def restore_routing_go(original: str):
    """Restore routing.go to original content."""
    ROUTING_GO.write_text(original)


def run_algo_phase(name: str, binary: Path, workloads: list[Path],
                   seeds: list[int], instances: int,
                   results: dict, algo_key: str,
                   routing_scorers: str = DEFAULT_ROUTING_SCORERS):
    """Run an algorithm across all workloads and seeds, storing results."""
    for wl in workloads:
        wl_name = wl.stem
        print(f"\n  Running {name} on {wl.name}...")
        for seed in seeds:
            if seed not in results[wl_name]:
                results[wl_name][seed] = {}
            metrics = run_blis(binary, wl, seed, instances,
                               routing_scorers=routing_scorers)
            if metrics:
                results[wl_name][seed][algo_key] = metrics
                print(f"    seed={seed}: e2e_mean={metrics['e2e_mean_ms']:.1f}ms, "
                      f"p95={metrics['e2e_p95_ms']:.1f}ms")
            else:
                print(f"    seed={seed}: FAILED")


def main():
    parser = argparse.ArgumentParser(description="Benchmark default vs glia vs oracle BLIS router")
    parser.add_argument("--seeds", default=",".join(str(s) for s in DEFAULT_SEEDS),
                        help="Comma-separated seeds (default: 42,123,456,789,1337)")
    parser.add_argument("--workloads", nargs="*", default=None,
                        help="Specific workload files to test (default: all in oracle/)")
    parser.add_argument("--instances", type=int, default=DEFAULT_INSTANCES,
                        help="Number of instances (default: 4)")
    args = parser.parse_args()

    seeds = [int(s) for s in args.seeds.split(",")]
    workloads = find_workloads(args.workloads)

    if not workloads:
        print("No workloads found!", file=sys.stderr)
        sys.exit(1)

    print(f"Workloads: {[w.name for w in workloads]}")
    print(f"Seeds: {seeds}")
    print(f"Instances: {args.instances}")
    print()

    # Pre-extract algorithm blocks
    oracle_block = _extract_block(ORACLE_GO, "// ORACLE-START", "// ORACLE-END")
    v2_block = _extract_block(ORACLE_V2_GO, "// V2-START", "// V2-END")
    glia_block = _extract_block(GLIA_GO, "// GLIA-START", "// GLIA-END")

    results = {}
    for wl in workloads:
        results[wl.stem] = {}

    # === Phase 1: Default router (no patching) ===
    print("=" * 60)
    print("Phase 1/4: Default router (pa:3, qd:2, kv:2)")
    print("=" * 60)
    default_binary = build_blis()
    run_algo_phase("default", default_binary, workloads, seeds, args.instances, results, "default")

    # === Phase 2: Glia HRA (patch + rebuild) ===
    print(f"\n{'=' * 60}")
    print("Phase 2/4: Glia HRA (KV headroom allocator)")
    print("=" * 60)
    print("Patching routing.go with Glia...")
    original = patch_routing_go(glia_block)
    try:
        glia_binary = build_blis()
        run_algo_phase("glia", glia_binary, workloads, seeds, args.instances, results, "glia")
    finally:
        print("\nRestoring routing.go...")
        restore_routing_go(original)

    # === Phase 3: Oracle v1 (patch + rebuild) ===
    print(f"\n{'=' * 60}")
    print("Phase 3/4: Oracle v1 (global IFR-based affinity decay)")
    print("=" * 60)
    print("Patching routing.go with Oracle v1...")
    original = patch_routing_go(oracle_block)
    try:
        oracle_binary = build_blis()
        run_algo_phase("oracle_v1", oracle_binary, workloads, seeds, args.instances, results, "oracle_v1")
    finally:
        print("\nRestoring routing.go...")
        restore_routing_go(original)

    # === Phase 4: v2 per-instance IFR penalty (patch + rebuild) ===
    print(f"\n{'=' * 60}")
    print("Phase 4/4: v2 (per-instance IFR penalty)")
    print("=" * 60)
    print("Patching routing.go with v2...")
    original = patch_routing_go(v2_block)
    try:
        v2_binary = build_blis()
        run_algo_phase("v2", v2_binary, workloads, seeds, args.instances, results, "v2")
    finally:
        print("\nRestoring routing.go...")
        restore_routing_go(original)
        build_blis()  # rebuild clean binary

    # === Report ===
    algos = ["default", "glia", "oracle_v1", "v2"]
    algo_labels = {
        "default": "Default (3:2:2)",
        "glia": "Glia HRA",
        "oracle_v1": "Oracle v1",
        "v2": "v2 (IFR penalty)",
    }

    print("\n")
    print("=" * 120)
    print("RESULTS: Default vs Glia vs Oracle v1 vs v2")
    print("=" * 120)

    for wl_name, seed_results in results.items():
        print(f"\n{'='*60}")
        print(f"Workload: {wl_name}")
        print(f"{'='*60}")

        # E2E Mean table
        print(f"\nE2E Mean Latency (ms):")
        print(f"{'Seed':>6}", end="")
        for algo in algos:
            print(f" | {algo_labels[algo]:>18}", end="")
        print()
        print("-" * 90)

        per_algo_e2e = {a: [] for a in algos}
        for seed in seeds:
            if seed not in seed_results:
                continue
            sr = seed_results[seed]
            print(f"{seed:>6}", end="")
            for algo in algos:
                if algo in sr:
                    val = sr[algo]["e2e_mean_ms"]
                    per_algo_e2e[algo].append(val)
                    print(f" | {val:>18.1f}", end="")
                else:
                    print(f" | {'FAILED':>18}", end="")
            print()

        means_e2e = {}
        print(f"{'MEAN':>6}", end="")
        for algo in algos:
            vals = per_algo_e2e[algo]
            if vals:
                m = sum(vals) / len(vals)
                means_e2e[algo] = m
                print(f" | {m:>18.1f}", end="")
            else:
                print(f" | {'N/A':>18}", end="")
        print()

        # P95 table
        print(f"\nE2E P95 Latency (ms):")
        print(f"{'Seed':>6}", end="")
        for algo in algos:
            print(f" | {algo_labels[algo]:>18}", end="")
        print()
        print("-" * 90)

        per_algo_p95 = {a: [] for a in algos}
        for seed in seeds:
            if seed not in seed_results:
                continue
            sr = seed_results[seed]
            print(f"{seed:>6}", end="")
            for algo in algos:
                if algo in sr:
                    val = sr[algo]["e2e_p95_ms"]
                    per_algo_p95[algo].append(val)
                    print(f" | {val:>18.1f}", end="")
                else:
                    print(f" | {'FAILED':>18}", end="")
            print()

        means_p95 = {}
        print(f"{'MEAN':>6}", end="")
        for algo in algos:
            vals = per_algo_p95[algo]
            if vals:
                m = sum(vals) / len(vals)
                means_p95[algo] = m
                print(f" | {m:>18.1f}", end="")
            else:
                print(f" | {'N/A':>18}", end="")
        print()

        # Improvement tables vs each baseline
        for baseline in ["default", "glia"]:
            if baseline not in means_e2e:
                continue
            bl_label = algo_labels[baseline]
            print(f"\n% Improvement vs {bl_label}:")
            print(f"{'Algorithm':>18} | {'E2E Mean':>12} | {'P95':>12} | {'Combined':>12}")
            print("-" * 60)
            for algo in algos:
                if algo == baseline:
                    continue
                if algo in means_e2e and algo in means_p95:
                    e2e_imp = (1 - means_e2e[algo] / means_e2e[baseline]) * 100
                    p95_imp = (1 - means_p95[algo] / means_p95[baseline]) * 100
                    combined = (e2e_imp + p95_imp) / 2
                    marker = " ***" if combined >= 30 else ""
                    print(f"{algo_labels[algo]:>18} | {e2e_imp:>+11.1f}% | {p95_imp:>+11.1f}% | {combined:>+11.1f}%{marker}")

    # Save JSON results
    output_path = SCRIPT_DIR / "benchmark_results.json"
    with open(output_path, "w") as f:
        json.dump(results, f, indent=2)
    print(f"\nDetailed results saved to: {output_path}")


if __name__ == "__main__":
    main()
