#!/usr/bin/env python3
"""
Parameter sweep for prefix_heavy workload: 1:1 vs Evolved.

Sweeps:
  1. Number of instances: [2, 3, 4, 6, 8]  (snapshot_refresh fixed at 5s)
  2. Snapshot refresh interval: [50ms, 250ms, 1s, 5s, 15s]  (instances fixed at 4)

Usage:
    cd sim2real/blis_router_analysis
    python sweep.py [--seeds 42,456]
"""

import argparse
import json
import shutil
import subprocess
import sys
from pathlib import Path

import matplotlib.pyplot as plt
import matplotlib.ticker as mticker

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------
SIM2REAL_DIR = Path(__file__).parent
INFERENCE_SIM_DIR = SIM2REAL_DIR / "inference-sim"
ROUTING_GO_PATH = INFERENCE_SIM_DIR / "sim" / "routing.go"
POLICY_CONFIG = SIM2REAL_DIR / "routing_config" / "routing_policy.yaml"
WORKLOAD_PATH = SIM2REAL_DIR / "workloads" / "workload_glia_prefix_heavy.yaml"

PROGRAMS = [
    ("1:1 (default)", SIM2REAL_DIR / "baselines" / "baseline_1_1.go",  True),
    ("Evolved (best)", SIM2REAL_DIR / "best"      / "best_program.go", False),
]

MODEL_ID = "qwen/qwen2.5-7b-instruct "
MODEL_EXTRA_ARGS = ["--hardware", "H100", "--tp", "1"]

# Sweep axes
INSTANCE_COUNTS  = [2, 3, 4, 6, 8]   # fixed refresh = 5s
REFRESH_CONFIGS  = [                   # fixed instances = 4
    ("50 ms",  "50000"),
    ("250 ms", "250000"),
    ("1 s",    "1000000"),
    ("5 s",    "5000000"),
    ("15 s",   "15000000"),
]

DEFAULT_INSTANCES = "4"
DEFAULT_REFRESH   = "5000000"


# ---------------------------------------------------------------------------
# Sim helpers
# ---------------------------------------------------------------------------

def build_sim(routing_go_src: Path) -> bool:
    shutil.copy2(routing_go_src, ROUTING_GO_PATH)
    r = subprocess.run(
        ["go", "build", "-o", "simulation_worker", "main.go"],
        cwd=INFERENCE_SIM_DIR, capture_output=True, text=True, timeout=60,
    )
    if r.returncode != 0:
        print(f"  [BUILD ERROR] {r.stderr.strip()[:200]}", file=sys.stderr)
        return False
    return True


def run_sim(seed: str, num_instances: str, snapshot_refresh: str) -> dict | None:
    cmd = [
        "./simulation_worker", "run",
        "--model", MODEL_ID,
        "--num-instances", num_instances,
        "--policy-config", str(POLICY_CONFIG),
        "--workload-spec", str(WORKLOAD_PATH),
        "--snapshot-refresh-interval", snapshot_refresh,
        "--log", "info",
        "--seed", seed,
    ] + MODEL_EXTRA_ARGS
    try:
        r = subprocess.run(
            cmd, cwd=INFERENCE_SIM_DIR, capture_output=True, text=True, timeout=120,
        )
    except subprocess.TimeoutExpired:
        print(f"  [TIMEOUT] seed={seed}", file=sys.stderr)
        return None
    if r.returncode != 0:
        print(f"  [SIM ERROR rc={r.returncode}] {r.stderr.strip()[:200]}", file=sys.stderr)
        return None
    return _parse_cluster_metrics(r.stdout + r.stderr)


def _parse_cluster_metrics(output: str) -> dict | None:
    in_json, buf, brace_count = False, "", 0
    for line in output.split("\n"):
        s = line.strip()
        if s.startswith("{"):
            in_json, brace_count = True, 0
        if in_json:
            buf += line + "\n"
            brace_count += s.count("{") - s.count("}")
            if brace_count == 0 and buf.strip():
                try:
                    block = json.loads(buf)
                    if block.get("instance_id") == "cluster":
                        return block
                except json.JSONDecodeError:
                    pass
                buf, in_json = "", False
    return None


def avg_metrics(seeds: list[str], num_instances: str, snapshot_refresh: str) -> dict | None:
    e2e_list, p95_list = [], []
    for seed in seeds:
        m = run_sim(seed, num_instances, snapshot_refresh)
        if m:
            e2e_list.append(float(m.get("e2e_mean_ms", 0)))
            p95_list.append(float(m.get("e2e_p95_ms", 0)))
    if not e2e_list:
        return None
    return {
        "e2e_ms": sum(e2e_list) / len(e2e_list),
        "p95_ms": sum(p95_list) / len(p95_list),
    }


def improvement(cand: dict, ctrl: dict) -> float:
    c = 0.5 * cand["e2e_ms"] + 0.5 * cand["p95_ms"]
    b = 0.5 * ctrl["e2e_ms"] + 0.5 * ctrl["p95_ms"]
    return (1.0 - c / b) * 100.0 if b > 0 else 0.0


# ---------------------------------------------------------------------------
# Run a sweep axis: list of (label, num_instances, snapshot_refresh)
# ---------------------------------------------------------------------------

def run_sweep(configs: list[tuple], seeds: list[str]) -> list[dict]:
    """
    configs: list of (label, num_instances_str, snapshot_refresh_str)
    Returns list of result rows.
    """
    original = ROUTING_GO_PATH.read_text()
    rows = []
    try:
        for label, n_inst, refresh in configs:
            row = {"label": label}
            built = {}
            for prog_name, prog_path, is_ctrl in PROGRAMS:
                print(f"  [{label}] {prog_name} ...", end=" ", flush=True)
                if prog_name not in built:
                    if not build_sim(prog_path):
                        built[prog_name] = None
                    else:
                        built[prog_name] = True
                if not built[prog_name]:
                    row[prog_name] = None
                    print("BUILD FAILED")
                    continue
                m = avg_metrics(seeds, n_inst, refresh)
                row[prog_name] = m
                if m:
                    print(f"e2e={m['e2e_ms']:.0f}ms  p95={m['p95_ms']:.0f}ms")
                else:
                    print("SIM FAILED")
            rows.append(row)
    finally:
        ROUTING_GO_PATH.write_text(original)
    return rows


# ---------------------------------------------------------------------------
# Print sweep table
# ---------------------------------------------------------------------------

def pct_improvement(cand_val: float, ctrl_val: float) -> float:
    return (1.0 - cand_val / ctrl_val) * 100.0 if ctrl_val > 0 else 0.0


def print_sweep_table(title: str, rows: list[dict], default_label: str):
    ctrl_name  = "1:1 (default)"
    evol_name  = "Evolved (best)"
    sep = "-" * 88
    print()
    print(f"{'=' * 88}")
    print(f"  {title}")
    print(f"{'=' * 88}")
    print(f"  {'Config':<12}  {'1:1 E2E':>9}  {'1:1 P95':>9}  {'Evol E2E':>9}  {'Evol P95':>9}  {'E2E improv':>11}  {'P95 improv':>11}")
    print(sep)
    for row in rows:
        label  = row["label"]
        marker = " *" if label == default_label else "  "
        ctrl   = row.get(ctrl_name)
        evol   = row.get(evol_name)
        if ctrl is None or evol is None:
            print(f"  {label + marker:<12}  FAILED")
            continue
        e2e_imp = pct_improvement(evol["e2e_ms"], ctrl["e2e_ms"])
        p95_imp = pct_improvement(evol["p95_ms"], ctrl["p95_ms"])
        print(
            f"  {label + marker:<12}"
            f"  {ctrl['e2e_ms']:>9.0f}"
            f"  {ctrl['p95_ms']:>9.0f}"
            f"  {evol['e2e_ms']:>9.0f}"
            f"  {evol['p95_ms']:>9.0f}"
            f"  {e2e_imp:>+10.1f}%"
            f"  {p95_imp:>+10.1f}%"
        )
    print(sep)
    print("  * = current default.  positive = evolved is faster than 1:1.")
    print()


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(description="prefix_heavy parameter sweep: 1:1 vs Evolved")
    parser.add_argument("--seeds", default="42,456")
    args = parser.parse_args()
    seeds = [s.strip() for s in args.seeds.split(",") if s.strip()]

    for p in [INFERENCE_SIM_DIR, ROUTING_GO_PATH, WORKLOAD_PATH]:
        if not p.exists():
            print(f"ERROR: missing {p}", file=sys.stderr)
            sys.exit(1)

    print(f"prefix_heavy parameter sweep")
    print(f"  seeds={seeds}  workload=workload_glia_prefix_heavy.yaml")
    print(f"  programs: 1:1 (default), Evolved (best)")

    # ---- Sweep 1: number of instances (refresh fixed at 5s) ----------------
    print(f"\n{'─'*60}")
    print(f"Sweep 1: instances  (snapshot_refresh=5s fixed)")
    print(f"{'─'*60}")
    inst_configs = [(str(n), str(n), DEFAULT_REFRESH) for n in INSTANCE_COUNTS]
    inst_rows = run_sweep(inst_configs, seeds)
    print_sweep_table(
        "Sweep 1 — instances (prefix_heavy, refresh=5s)",
        inst_rows,
        default_label=DEFAULT_INSTANCES,
    )

    # ---- Sweep 2: snapshot refresh interval (instances fixed at 4) ----------
    print(f"{'─'*60}")
    print(f"Sweep 2: snapshot refresh  (instances=4 fixed)")
    print(f"{'─'*60}")
    ref_configs = [(label, DEFAULT_INSTANCES, us) for label, us in REFRESH_CONFIGS]
    ref_rows = run_sweep(ref_configs, seeds)
    print_sweep_table(
        "Sweep 2 — snapshot refresh (prefix_heavy, instances=4)",
        ref_rows,
        default_label="5 s",
    )

    plot_sweeps(inst_rows, ref_rows)


def plot_sweeps(inst_rows: list[dict], ref_rows: list[dict]):
    ctrl_name = "1:1 (default)"
    evol_name = "Evolved (best)"

    def extract(rows):
        labels, e2e_imp, p95_imp = [], [], []
        for row in rows:
            ctrl = row.get(ctrl_name)
            evol = row.get(evol_name)
            if ctrl and evol:
                labels.append(row["label"])
                e2e_imp.append(pct_improvement(evol["e2e_ms"], ctrl["e2e_ms"]))
                p95_imp.append(pct_improvement(evol["p95_ms"], ctrl["p95_ms"]))
        return labels, e2e_imp, p95_imp

    inst_labels, inst_e2e, inst_p95 = extract(inst_rows)
    ref_labels,  ref_e2e,  ref_p95  = extract(ref_rows)

    fig, axes = plt.subplots(1, 2, figsize=(13, 5))
    fig.suptitle("prefix_heavy: Evolved vs 1:1 improvement", fontsize=13, fontweight="bold")

    x_colors = ["#4C72B0", "#DD8452"]  # blue=E2E, orange=P95

    for ax, labels, e2e_vals, p95_vals, xlabel, default_label, title in [
        (axes[0], inst_labels, inst_e2e, inst_p95, "Number of instances", "4",
         "Sweep 1 — instances (refresh=5s)"),
        (axes[1], ref_labels,  ref_e2e,  ref_p95,  "Snapshot refresh interval", "5 s",
         "Sweep 2 — snapshot refresh (instances=4)"),
    ]:
        x = range(len(labels))
        w = 0.35
        bars_e2e = ax.bar([i - w/2 for i in x], e2e_vals, w, label="E2E improv", color=x_colors[0], alpha=0.85)
        bars_p95 = ax.bar([i + w/2 for i in x], p95_vals, w, label="P95 improv", color=x_colors[1], alpha=0.85)

        # value labels on bars
        for bar in list(bars_e2e) + list(bars_p95):
            h = bar.get_height()
            ax.text(bar.get_x() + bar.get_width() / 2, h + 0.3, f"{h:.1f}%",
                    ha="center", va="bottom", fontsize=8)

        ax.set_xticks(list(x))
        ax.set_xticklabels(
            [f"{l}\n(default)" if l == default_label else l for l in labels],
            fontsize=9,
        )
        ax.set_xlabel(xlabel, fontsize=10)
        ax.set_ylabel("Improvement over 1:1 (%)", fontsize=10)
        ax.set_title(title, fontsize=10)
        ax.yaxis.set_major_formatter(mticker.FormatStrFormatter("%.0f%%"))
        ax.axhline(0, color="black", linewidth=0.6, linestyle="--")
        ax.legend(fontsize=9)
        ax.set_ylim(0, max(max(e2e_vals), max(p95_vals)) * 1.25)
        ax.grid(axis="y", alpha=0.3)

    plt.tight_layout()
    out_path = SIM2REAL_DIR / "sweep_results.png"
    plt.savefig(out_path, dpi=150, bbox_inches="tight")
    print(f"\nChart saved to {out_path}")
    plt.show()


if __name__ == "__main__":
    main()
