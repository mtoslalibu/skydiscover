#!/usr/bin/env python3
"""
Benchmark: Oracle adaptive router vs Default BLIS router.

Runs both routers against all workload YAMLs and reports E2E mean/P95
improvement across multiple seeds.

Usage:
    python benchmark.py                          # run with defaults
    python benchmark.py --seeds 42,123,456       # custom seeds
    python benchmark.py --workloads workload_v1.yaml  # single workload
    python benchmark.py --instances 4            # custom instance count
"""

import argparse
import json
import os
import re
import subprocess
import sys
import textwrap
from pathlib import Path

SCRIPT_DIR = Path(__file__).resolve().parent
BLIS_ROUTER_DIR = SCRIPT_DIR.parent
SIM_DIR = BLIS_ROUTER_DIR / "inference-sim"
ROUTING_GO = SIM_DIR / "sim" / "routing.go"
ORACLE_GO = SCRIPT_DIR / "oracle_router.go"
# Use the local oracle copy of the workload (workload_v1.yaml in this directory).
# Falls back to the shared workloads/ dir if no local copy exists.
WORKLOADS_DIR = SCRIPT_DIR  # oracle/ directory contains workload_v1.yaml

DEFAULT_SEEDS = [42, 123, 456, 789, 1337]
DEFAULT_INSTANCES = 4
DEFAULT_SNAPSHOT_REFRESH = 5_000_000  # 5s in microseconds

# Markers in routing.go that delimit the replaceable block
BLOCK_START = "// Compute composite scores from all scorers"
# End marker: the closing brace of the "if len(tied) > 1" block, one line
# after the bestIdx assignment. We match this line to capture the full block.
BLOCK_END_MARKER = "bestIdx = tied[ws.rng.Intn(len(tied))]"
# We need to skip one extra line after the marker (the closing "}")
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
    subprocess.run(
        ["go", "build", "-o", str(binary), "main.go"],
        cwd=SIM_DIR, check=True, capture_output=True, text=True, timeout=60,
    )
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
    # Split on JSON object boundaries and find cluster block
    for match in re.finditer(r'\{[^{}]+\}', output):
        try:
            obj = json.loads(match.group())
            if obj.get("instance_id") == "cluster":
                return obj
        except json.JSONDecodeError:
            continue
    return None


def patch_routing_go_oracle():
    """Replace the scoring block in routing.go with oracle code."""
    original = ROUTING_GO.read_text()
    oracle_code = ORACLE_GO.read_text()

    # Extract only the oracle algorithm block between ORACLE-START and ORACLE-END markers.
    # oracle_router.go is a full routing.go copy; we only want the replacement block.
    oracle_start_marker = "// ORACLE-START"
    oracle_end_marker = "// ORACLE-END"
    os_idx = oracle_code.find(oracle_start_marker)
    oe_idx = oracle_code.find(oracle_end_marker)
    if os_idx == -1 or oe_idx == -1:
        raise RuntimeError("Could not find ORACLE-START/ORACLE-END markers in oracle_router.go")
    # Include everything from ORACLE-START line through the line before ORACLE-END
    os_line_start = oracle_code.rfind('\n', 0, os_idx) + 1
    oe_line_end = oracle_code.index('\n', oe_idx) + 1
    oracle_body = oracle_code[os_line_start:oe_line_end].rstrip()

    # The oracle body is already indented with tabs from the full file — use as-is.
    indented = oracle_body

    # Find and replace the block between BLOCK_START and BLOCK_END_MARKER
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

    patched = original[:line_start] + indented + '\n' + original[end_idx:]
    ROUTING_GO.write_text(patched)
    return original


def restore_routing_go(original: str):
    """Restore routing.go to original content."""
    ROUTING_GO.write_text(original)


def main():
    parser = argparse.ArgumentParser(description="Benchmark oracle vs default BLIS router")
    parser.add_argument("--seeds", default=",".join(str(s) for s in DEFAULT_SEEDS),
                        help="Comma-separated seeds (default: 42,123,456,789,1337)")
    parser.add_argument("--workloads", nargs="*", default=None,
                        help="Specific workload files to test (default: all in workloads/)")
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

    # === Phase 1: Build default binary and run baselines ===
    print("Building default BLIS...")
    default_binary = build_blis()

    results = {}  # {workload_name: {seed: {default: metrics, oracle: metrics}}}

    for wl in workloads:
        wl_name = wl.stem
        results[wl_name] = {}
        print(f"\n{'='*60}")
        print(f"Workload: {wl.name}")
        print(f"{'='*60}")

        # Run default router
        print("  Running default (pa:3, qd:2, kv:2)...")
        for seed in seeds:
            metrics = run_blis(default_binary, wl, seed, args.instances)
            if metrics:
                results[wl_name][seed] = {"default": metrics}
                print(f"    seed={seed}: e2e_mean={metrics['e2e_mean_ms']:.1f}ms, "
                      f"p95={metrics['e2e_p95_ms']:.1f}ms")
            else:
                print(f"    seed={seed}: FAILED")

    # === Phase 2: Patch routing.go with oracle, rebuild, run ===
    print("\n\nPatching routing.go with oracle...")
    original_routing = patch_routing_go_oracle()
    try:
        print("Building oracle BLIS...")
        oracle_binary = build_blis()

        for wl in workloads:
            wl_name = wl.stem
            print(f"\n  Running oracle on {wl.name}...")
            for seed in seeds:
                if seed not in results[wl_name]:
                    continue
                metrics = run_blis(oracle_binary, wl, seed, args.instances)
                if metrics:
                    results[wl_name][seed]["oracle"] = metrics
                    print(f"    seed={seed}: e2e_mean={metrics['e2e_mean_ms']:.1f}ms, "
                          f"p95={metrics['e2e_p95_ms']:.1f}ms")
                else:
                    print(f"    seed={seed}: FAILED")
    finally:
        print("\nRestoring routing.go...")
        restore_routing_go(original_routing)
        # Rebuild clean binary
        build_blis()

    # === Phase 3: Report ===
    print("\n")
    print("=" * 80)
    print("RESULTS: Oracle vs Default BLIS Router")
    print("=" * 80)

    for wl_name, seed_results in results.items():
        print(f"\n--- {wl_name} ---")
        print(f"{'Seed':>6} | {'Default E2E':>12} | {'Oracle E2E':>11} | {'Imp %':>7} | "
              f"{'Default P95':>12} | {'Oracle P95':>11} | {'Imp %':>7}")
        print("-" * 80)

        e2e_improvements = []
        p95_improvements = []

        for seed in seeds:
            if seed not in seed_results:
                continue
            sr = seed_results[seed]
            if "default" not in sr or "oracle" not in sr:
                continue

            d_e2e = sr["default"]["e2e_mean_ms"]
            o_e2e = sr["oracle"]["e2e_mean_ms"]
            d_p95 = sr["default"]["e2e_p95_ms"]
            o_p95 = sr["oracle"]["e2e_p95_ms"]

            e2e_imp = (1 - o_e2e / d_e2e) * 100
            p95_imp = (1 - o_p95 / d_p95) * 100
            e2e_improvements.append(e2e_imp)
            p95_improvements.append(p95_imp)

            print(f"{seed:>6} | {d_e2e:>10.1f}ms | {o_e2e:>9.1f}ms | {e2e_imp:>+6.1f}% | "
                  f"{d_p95:>10.1f}ms | {o_p95:>9.1f}ms | {p95_imp:>+6.1f}%")

        if e2e_improvements:
            mean_e2e = sum(e2e_improvements) / len(e2e_improvements)
            mean_p95 = sum(p95_improvements) / len(p95_improvements)
            print("-" * 80)
            print(f"{'MEAN':>6} | {'':>12} | {'':>11} | {mean_e2e:>+6.1f}% | "
                  f"{'':>12} | {'':>11} | {mean_p95:>+6.1f}%")

    # Save JSON results
    output_path = SCRIPT_DIR / "benchmark_results.json"
    with open(output_path, "w") as f:
        json.dump(results, f, indent=2)
    print(f"\nDetailed results saved to: {output_path}")


if __name__ == "__main__":
    main()
