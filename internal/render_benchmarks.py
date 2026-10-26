"""Render the checked-in Go/Rust report as SVG charts and README tables.

Requires matplotlib; run from the repository root.
"""
import json
import math
from collections import defaultdict
from pathlib import Path
from statistics import median

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.patches import Patch

ROOT = Path(__file__).resolve().parent.parent
REPORT = ROOT / "benchmarks/arm64-2026-10-26.json"
SIZES = [4096, 262144, 5000000, 50000000]
LABELS = ["4K", "262K", "5M", "50M"]
CASES = [
    ("hit", "Read hit"), ("miss", "Read miss"),
    ("batch8_hit", "Batch hit · 8 keys"), ("batch8_miss", "Batch miss · 8 keys"),
    ("replace", "Replace value"), ("churn_pair", "Delete + insert"),
    ("parallel4_hit", "4 workers · reads"),
    ("parallel4_write10", "4 workers · 10% writes"),
]
GO, RUST = "#148575", "#c45b40"
INK, MUTED, GRID, BG = "#172b43", "#5e7084", "#e2e8ef", "#f7f9fc"


def main():
    report = json.loads(REPORT.read_text())
    cpu, rss = defaultdict(list), defaultdict(list)
    for row in report["cpu"]:
        if row["language"] in ("go", "rust") and row["kind"] in ("tree", "map"):
            for case in row["cases"]:
                name = case["name"][len(row["kind"]) + 1:]
                cpu[row["entries"], row["kind"], row["language"], name].append(case["ns_per_op"])
    for row in report["rss"]:
        if row["language"] in ("go", "rust"):
            rss[row["stages"][0]["entries"], row["kind"], row["language"]].append(row)
    assert all(len(values) == 3 for values in cpu.values())
    assert len(rss) == 16 and all(len(values) == 3 for values in rss.values())

    def time(size, kind, language, case):
        return median(cpu[size, kind, language, case])

    def memory(size, kind, language, metric):
        rows = rss[size, kind, language]
        if metric == "build":
            return median(r["stages"][0]["build_ns"] for r in rows) / 1e9
        if metric == "peak":
            return median(r["peak_rss_bytes"] for r in rows) / 1e6
        index = {"built": 0, "read": 1, "settled": 2}[metric]
        return median(r["stages"][index]["rss_bytes"] for r in rows) / 1e6

    plt.rcParams.update({
        "font.family": "DejaVu Sans", "font.size": 12,
        "text.color": INK, "axes.labelcolor": MUTED,
        "xtick.color": MUTED, "ytick.color": INK,
        "svg.fonttype": "path", "svg.hashsalt": "gobbtree-benchmarks",
    })

    def save(fig, name, title):
        path = ROOT / "benchmarks" / f"{name}.svg"
        fig.savefig(path, facecolor=BG,
                    metadata={"Date": None, "Title": title,
                              "Creator": "gobbtree benchmark renderer"})
        path.write_text("\n".join(line.rstrip() for line in path.read_text().splitlines()) + "\n")
        plt.close(fig)

    def style(ax):
        ax.set_facecolor("white")
        for spine in ax.spines.values():
            spine.set_visible(False)
        ax.tick_params(length=0, pad=8)
        ax.set_axisbelow(True)

    fig, axes = plt.subplots(2, 4, figsize=(1200/72, 850/72))
    fig.subplots_adjust(left=.195, right=.97, top=.79, bottom=.12, hspace=.58, wspace=.22)
    fig.text(.04, .945, "Go vs Rust: operation time", fontsize=27, weight="bold")
    fig.text(.04, .906, "Apple M4 Max · Go 1.27.3 · Rust 1.97.1 · median of 3 runs", color=MUTED, fontsize=13)
    fig.legend(handles=[Patch(color=GO, label="Go faster"), Patch(color=RUST, label="Rust faster")],
               loc="upper left", bbox_to_anchor=(.035, .88), ncol=2, frameon=False, fontsize=12)
    ratios = [time(s, k, "rust", c) / time(s, k, "go", c)
              for s in SIZES for k in ("tree", "map") for c, _ in CASES[:6 if k == "tree" else 8]]
    limit = max(2.5, math.ceil(max(ratios) * 1.16 * 2) / 2)
    for row, kind in enumerate(("tree", "map")):
        cases = CASES[:6 if kind == "tree" else 8]
        for col, (size, label) in enumerate(zip(SIZES, LABELS)):
            ax = axes[row, col]
            style(ax)
            values = [time(size, kind, "rust", c) / time(size, kind, "go", c) for c, _ in cases]
            ax.barh(range(len(cases)), values, height=.58,
                    color=[GO if v >= 1 else RUST for v in values])
            ax.set_yticks(range(len(cases)), [label for _, label in cases] if col == 0 else [])
            ax.set_ylim(len(cases)-.4, -.6)
            ax.set_xlim(0, limit)
            ax.set_xticks([0, 1, 2], ["0", "1×", "2×"])
            ax.grid(axis="x", color=GRID, linewidth=.8)
            ax.axvline(1, color=INK, linewidth=1, linestyle=(0, (3, 3)))
            ax.set_title(f"{label} entries", fontsize=14, weight="bold", pad=16)
            for y, value in enumerate(values):
                ax.text(value + .045, y, f"{value:.2f}×", va="center", fontsize=11, weight="bold",
                        bbox={"facecolor": "white", "edgecolor": "none", "pad": .6})
        fig.text(.04, axes[row, 0].get_position().y1 + .04,
                 "TREE · single owner" if kind == "tree" else "MAP · concurrent", fontsize=15, weight="bold")
    fig.text(.195, .062, "Relative speed = Rust ns/op ÷ Go ns/op.  1× is equal; higher favors Go.", fontsize=12)
    fig.text(.195, .031, "Host CPU activity was present; small gaps are inconclusive. Source: 55907ff / 8adae97.", color=MUTED, fontsize=10.5)
    save(fig, "cpu-arm64", "Go versus Rust operation time by size and workload")

    fig, axes = plt.subplots(2, 4, figsize=(1200/72, 760/72))
    fig.subplots_adjust(left=.08, right=.97, top=.77, bottom=.17, hspace=.7, wspace=.36)
    fig.text(.04, .945, "Go vs Rust: memory and construction", fontsize=27, weight="bold")
    fig.text(.04, .9, "Whole-process RSS includes the runtime, GC headroom, and populated read caches.", color=MUTED, fontsize=13)
    fig.legend(handles=[Patch(color=GO, label="Go"), Patch(color=RUST, label="Rust")],
               loc="upper left", bbox_to_anchor=(.035, .876), ncol=2, frameon=False, fontsize=12)
    for row, metric in enumerate(("peak", "build")):
        for col, (size, label) in enumerate(zip(SIZES, LABELS)):
            ax = axes[row, col]
            style(ax)
            maximum = max(memory(size, k, l, metric) for k in ("tree", "map") for l in ("go", "rust"))
            for language, offset, color in (("go", -.18, GO), ("rust", .18, RUST)):
                values = [memory(size, k, language, metric) for k in ("tree", "map")]
                positions = [i + offset for i in range(2)]
                ax.bar(positions, values, width=.31, color=color)
                for x, value in zip(positions, values):
                    text = f"{value:,.1f}" if metric == "peak" and value >= 100 else f"{value:.2f}"
                    if metric == "build" and value < .01:
                        text = f"{value:.4f}"
                    ax.text(x, value + maximum*.035, text, ha="center", va="bottom", fontsize=10)
            ax.set_xticks([0, 1], ["Tree", "Map"])
            ax.set_ylim(0, maximum*1.3)
            ax.grid(axis="y", color=GRID, linewidth=.8)
            ax.set_title(f"{label} entries", fontsize=14, weight="bold", pad=16)
            if col == 0:
                ax.set_ylabel("Peak RSS · MB" if metric == "peak" else "Build time · seconds", fontsize=12)
    fig.text(.08, .095, "Lower is better. Each panel starts at zero and uses its own scale. MB are decimal.", fontsize=12)
    fig.text(.08, .063, "Apple M4 Max · Go 1.27.3 · Rust 1.97.1 · median of 3 fresh processes per case", color=MUTED, fontsize=10.5)
    fig.text(.08, .03, "Source: Go 55907ff / Rust 8adae97. Host CPU activity can affect construction times.", color=MUTED, fontsize=10.5)
    save(fig, "memory-arm64", "Go versus Rust peak memory and construction time")

    text = ["## Benchmarks", "",
            "Apple M4 Max (ARM64), Go 1.27.3 with Go SIMD, and Rust 1.97.1 with native CPU tuning, full LTO, and mimalloc. "
            "Measured sources: [Go `55907ff`](https://github.com/thesyncim/gobbtree/commit/55907ff08815d434de96af13b89e75e603680dd5) "
            "and [Rust `8adae97`](https://github.com/XiangpengHao/bbtree/tree/8adae97a0ed97e5c98fa2790312eb0aec9719846). "
            "Timings and memory figures are medians of three fresh-process runs using identical datasets and checked results. "
            "4K and 262K denote 4,096 and 262,144 entries.", "",
            "### Operation time", "",
            "![Go versus Rust speed ratios across four sizes and eight workloads](benchmarks/cpu-arm64.svg)", "",
            "The chart shows **Rust time / Go time**: above 1× favors Go; below 1× favors Rust. "
            "Batch timings are per key. Delete + insert is one pair. Parallel cases use four workers; the mixed case replaces existing values on 10% of operations.", "",
            "<details>", "<summary>Exact timings — Go / Rust, ns/op; lower is better</summary>", ""]
    for kind in ("tree", "map"):
        text += [f"**{kind.title()}**", "", "| Workload | 4K | 262K | 5M | 50M |", "|---|---:|---:|---:|---:|"]
        for case, label in CASES[:6 if kind == "tree" else 8]:
            cells = [f"{time(size, kind, 'go', case):.2f} / {time(size, kind, 'rust', case):.2f}" for size in SIZES]
            text.append("| " + label + " | " + " | ".join(cells) + " |")
        text.append("")
    text += ["</details>", "", "### Memory and construction", "",
             "![Go versus Rust peak RSS and construction time for Tree and Map at four sizes](benchmarks/memory-arm64.svg)", "",
             "Memory includes each process’s runtime, allocator, GC headroom, and caches populated by a full read pass. "
             "Go uses its default GC settings. Construction measures inserting all entries into an empty container.", "",
             "<details>", "<summary>Memory and build measurements — each cell is Go / Rust</summary>", "",
             "| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |",
             "|---|---:|---:|---:|---:|---:|"]
    for kind in ("tree", "map"):
        for size, label in zip(SIZES, LABELS):
            cells = []
            for metric in ("peak", "read", "settled", "build"):
                precision = 4 if metric == "build" else 2
                cells.append(" / ".join(f"{memory(size, kind, language, metric):,.{precision}f}" for language in ("go", "rust")))
            text.append(f"| {kind.title()} | {label} | " + " | ".join(cells) + " |")
    text += ["", "</details>", "",
             "Background CPU activity was observed on the host; small timing gaps are inconclusive. "
             "These are ARM64 measurements. Native amd64 results are available as [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml). "
             "The [raw report](benchmarks/arm64-2026-10-26.json) also includes Go’s built-in `map` and `sync.Map` CPU comparisons through 5M entries.", ""]
    readme = ROOT / "README.md"
    old = readme.read_text()
    start = old.index("## Benchmarks")
    end = old.index("## Reproduce the comparisons", start)
    readme.write_text(old[:start] + "\n".join(text) + "\n" + old[end:])


if __name__ == "__main__":
    main()
