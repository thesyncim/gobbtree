"""Render the checked-in Go/Rust report as SVG charts and README tables.

Requires matplotlib; run from the repository root.
"""
import argparse
import json
import re
from collections import defaultdict
from pathlib import Path
from statistics import median

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.patches import Patch, Rectangle

ROOT = Path(__file__).resolve().parent.parent
REPORT = ROOT / "benchmarks/arm64-2026-10-29.json"
SIZES = [4096, 262144, 5000000, 50000000]
LABELS = ["4K", "262K", "5M", "50M"]
CASES = [
    ("hit", "Read hit"), ("miss", "Read miss"),
    ("batch8_hit", "Batch hit · 8 keys"), ("batch8_miss", "Batch miss · 8 keys"),
    ("replace", "Replace value"), ("churn_pair", "Delete + reinsert same key"),
    ("churn_new_pair", "Delete + insert new key"),
    ("parallel4_hit", "4 workers · reads"),
    ("parallel4_write10", "4 workers · 10% writes"),
]
# Key patterns: name, heading, and the suffix added to chart file names.
PATTERNS = [
    ("mixed", "Mixed keys", ""),
    ("sequential", "Sequential keys", "-sequential"),
    ("clustered", "Clustered keys", "-clustered"),
]
PATTERN_NOTES = {
    "mixed": "hashed keys spread over the full 64-bit range",
    "sequential": "consecutive even keys; misses and fresh keys fill the gaps",
    "clustered": "runs of 1,024 consecutive even keys at scattered bases",
}
GO, RUST = "#148575", "#c45b40"
INK, MUTED, GRID, BG = "#172b43", "#5e7084", "#e2e8ef", "#f7f9fc"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path, default=REPORT)
    parser.add_argument("--preview-dir", type=Path)
    args = parser.parse_args()
    report = json.loads(args.report.read_text())
    go_source, rust_source = report["go_source"], report["rust_source"]
    go_short, rust_short = go_source[:7], rust_source[:7]
    go_version = report["toolchains"]["go"].removeprefix("go")
    rust_version = report["toolchains"]["rust"]
    report_link = args.report.resolve().relative_to(ROOT).as_posix()
    cpu, rss = defaultdict(list), defaultdict(list)
    for row in report["cpu"]:
        if row["language"] in ("go", "rust") and row["kind"] in ("tree", "map"):
            for case in row["cases"]:
                name = case["name"][len(row["kind"]) + 1:]
                cpu[row.get("keys", "mixed"), row["entries"], row["kind"], row["language"], name].append(case["ns_per_op"])
    for row in report["rss"]:
        if row["language"] in ("go", "rust"):
            rss[row.get("keys", "mixed"), row["stages"][0]["entries"], row["kind"], row["language"]].append(row)
    patterns = [p for p in PATTERNS if any(key[0] == p[0] for key in cpu)]
    available = {key[4] for key in cpu}
    cases = {kind: [(case, label) for case, label in CASES
                    if case in available and (kind == "map" or not case.startswith("parallel"))]
             for kind in ("tree", "map")}
    extra_tree = 48*(len(cases["tree"])-6)
    extra_total = extra_tree + 48*(len(cases["map"])-8)
    assert all(len(values) == 3 for values in cpu.values())
    assert len(rss) == 16*len(patterns) and all(len(values) == 3 for values in rss.values())

    def time(pattern, size, kind, language, case):
        return median(cpu[pattern, size, kind, language, case])

    def memory(pattern, size, kind, language, metric):
        rows = rss[pattern, size, kind, language]
        if metric == "build":
            return median(r["stages"][0]["build_ns"] for r in rows) / 1e9
        if metric == "peak":
            return median(r["peak_rss_bytes"] for r in rows) / 1e6
        index = {"built": 0, "read": 1, "settled": 2}[metric]
        return median(r["stages"][index]["rss_bytes"] for r in rows) / 1e6

    plt.rcParams.update({
        "font.family": "DejaVu Sans", "font.size": 16,
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
        if args.preview_dir:
            args.preview_dir.mkdir(parents=True, exist_ok=True)
            fig.savefig(args.preview_dir / f"{name}.png", facecolor=BG, dpi=100)
        plt.close(fig)

    # One matrix keeps every workload label next to its four comparisons.
    for pattern, heading, suffix in patterns:
        fig = plt.figure(figsize=(900/72, (1115+extra_total)/72))
        ax = fig.add_axes([0, 0, 1, 1])
        ax.set(xlim=(0, 900), ylim=(1115+extra_total, 0))
        ax.axis("off")

        def label(x, y, value, size=17, color=INK, weight="normal", align="left"):
            ax.text(x, y, value, fontsize=size, color=color, weight=weight,
                    ha=align, va="center")

        label(40, 42, "Go vs Rust: operation time", size=28, weight="bold")
        label(40, 83, "Change in Go time vs Rust. Negative means faster Go.", size=18)
        label(40, 116, f"{heading} · Apple M4 Max · median of 3 fresh-process runs", size=16, color=MUTED)
        for x, color, title in [(40, "#d1eee3", "Go uses less time"),
                                 (335, "#e8edf2", "Within 5%"),
                                 (560, "#f8dfd3", "Go uses more time")]:
            ax.add_patch(Rectangle((x, 146), 19, 19, facecolor=color, edgecolor="none"))
            label(x+29, 156, title, size=16)
        label(40, 199, "WORKLOAD", size=14, color=MUTED, weight="bold")
        centers = [407, 537, 667, 797]
        for x, name in zip(centers, LABELS):
            label(x, 199, name + " entries", size=17, weight="bold", align="center")
        for kind, top, first in [("tree", 238, 282), ("map", 588+extra_tree, 632+extra_tree)]:
            label(40, top, "TREE · single owner" if kind == "tree" else "MAP · concurrent",
                  size=20, weight="bold")
            for row, (case, title) in enumerate(cases[kind]):
                y = first + row*48
                label(40, y, title)
                for x, size in zip(centers, SIZES):
                    change = 100*(time(pattern, size, kind, "go", case)/time(pattern, size, kind, "rust", case)-1)
                    fill, ink = ("#d1eee3", "#095d48") if change < -5 else ("#f8dfd3", "#922f12")
                    if abs(change) <= 5:
                        fill, ink = "#e8edf2", "#405469"
                    ax.add_patch(Rectangle((x-59, y-21), 118, 42, facecolor=fill, edgecolor="none"))
                    value = "0%" if round(change, 1) == 0 else f"{change:+.1f}%"
                    label(x, y, value, size=22, color=ink, weight="bold", align="center")
        label(40, 1030+extra_total, "Batch: per key · delete + insert: one pair · parallel: 4 workers", size=14)
        label(40, 1058+extra_total, "Small timing gaps are inconclusive: host CPU activity was present.", size=14, color=MUTED)
        label(40, 1087+extra_total, f"Go {go_version} / {go_short} · Rust {rust_version} / {rust_short} · (Go time ÷ Rust time − 1) × 100", size=13, color=MUTED)
        save(fig, f"cpu{suffix}-arm64", f"Percentage change in Go operation time versus Rust, {heading.lower()}")

    # Four roomy panels per figure; keep memory and build time separate.
    for pattern, heading, suffix in patterns:
        for metric in ("peak", "build"):
            fig, axes = plt.subplots(2, 2, figsize=(900/72, 900/72))
            fig.subplots_adjust(left=.095, right=.96, top=.755, bottom=.16, hspace=.65, wspace=.32)
            title = "Go vs Rust: peak memory" if metric == "peak" else "Go vs Rust: construction time"
            fig.text(.04, .945, title, fontsize=28, weight="bold")
            subtitle = "Whole-process RSS in MB · lower is better" if metric == "peak" else "Time to insert all entries · lower is better"
            fig.text(.04, .9, f"{subtitle} · {heading.lower()}", fontsize=18)
            fig.legend(handles=[Patch(color=GO, label="Go"), Patch(color=RUST, label="Rust")],
                       loc="upper left", bbox_to_anchor=(.025, .878), ncol=2, frameon=False, fontsize=18)
            for ax, size, name in zip(axes.flat, SIZES, LABELS):
                ax.set_facecolor("white")
                for spine in ax.spines.values():
                    spine.set_visible(False)
                ax.tick_params(length=0, pad=8)
                ax.set_axisbelow(True)
                factor = 1000 if metric == "build" and size < 1000000 else 1
                unit = "MB" if metric == "peak" else "ms" if factor == 1000 else "s"
                maximum = max(memory(pattern, size, k, l, metric)*factor for k in ("tree", "map") for l in ("go", "rust"))
                for language, offset, color in (("go", -.18, GO), ("rust", .18, RUST)):
                    values = [memory(pattern, size, k, language, metric)*factor for k in ("tree", "map")]
                    positions = [i + offset for i in range(2)]
                    ax.bar(positions, values, width=.31, color=color)
                    for x, value in zip(positions, values):
                        text = f"{value:,.1f}" if value >= 10 else f"{value:.2f}"
                        ax.text(x, value + maximum*.045, text, ha="center", va="bottom", fontsize=17)
                ax.set_xticks([0, 1], ["Tree", "Map"])
                ax.set_ylim(0, maximum*1.32)
                ax.locator_params(axis="y", nbins=4)
                ax.grid(axis="y", color=GRID, linewidth=.8)
                ax.set_title(f"{name} entries · {unit}", fontsize=20, weight="bold", pad=19)
            fig.text(.095, .09, "Each panel starts at zero and uses its own scale.", fontsize=15)
            detail = "Includes the runtime, GC headroom, and populated read caches." if metric == "peak" else "Construction timings are subject to background CPU activity."
            fig.text(.095, .06, detail, fontsize=14, color=MUTED)
            fig.text(.095, .025, f"M4 Max · 3 runs · Go {go_version} / {go_short} · Rust {rust_version} / {rust_short}", fontsize=13, color=MUTED)
            save(fig, f"{'memory' if metric == 'peak' else 'build'}{suffix}-arm64", f"{title}, {heading.lower()}")

    def timing_tables(pattern):
        rows = []
        for kind in ("tree", "map"):
            rows += [f"**{kind.title()}**", "", "| Workload | 4K | 262K | 5M | 50M |", "|---|---:|---:|---:|---:|"]
            for case, label in cases[kind]:
                cells = [f"{time(pattern, size, kind, 'go', case):.2f} / {time(pattern, size, kind, 'rust', case):.2f}" for size in SIZES]
                rows.append("| " + label + " | " + " | ".join(cells) + " |")
            rows.append("")
        return rows

    def memory_table(pattern):
        rows = ["| Container | Entries | Peak RSS, MB | After reads, MB | Collected / settled RSS, MB | Build, seconds |",
                "|---|---:|---:|---:|---:|---:|"]
        for kind in ("tree", "map"):
            for size, label in zip(SIZES, LABELS):
                cells = []
                for metric in ("peak", "read", "settled", "build"):
                    precision = 4 if metric == "build" else 2
                    cells.append(" / ".join(f"{memory(pattern, size, kind, language, metric):,.{precision}f}" for language in ("go", "rust")))
                rows.append(f"| {kind.title()} | {label} | " + " | ".join(cells) + " |")
        return rows

    text = ["## Benchmarks", "",
            f"Go {go_version} with `GOEXPERIMENT=simd` against the Rust original ({rust_version}, native CPU tuning, full LTO, mimalloc) "
            f"on an Apple M4 Max. Sources: [Go `{go_short}`](https://github.com/thesyncim/gobbtree/commit/{go_source}), "
            f"[Rust `{rust_short}`](https://github.com/XiangpengHao/bbtree/tree/{rust_source}). "
            "`uint64` keys and values; medians of three fresh-process runs over identical datasets, with checked results. "
            "Every workload runs with three key patterns: " +
            "; ".join(f"**{heading.lower()}**, {PATTERN_NOTES[pattern]}" for pattern, heading, _ in patterns) + ".", "",
            "![Change in Go operation time versus Rust across four sizes and multiple workloads, mixed keys](benchmarks/cpu-arm64.svg)", "",
            "Each cell is the **change in Go time versus Rust**: negative means Go is faster; gray cells are within 5%. "
            "Batches are per key. Churn is one deletion plus one insertion, of the same key or of a fresh one. "
            "Parallel rows use four workers; the mixed row replaces values on 10% of operations.", "",
            "<details>", "<summary>Exact timings, mixed keys — Go / Rust, ns/op; lower is better</summary>", ""]
    text += timing_tables("mixed")
    text += ["</details>", "",
             "![Go versus Rust peak RSS for Tree and Map at four sizes, mixed keys](benchmarks/memory-arm64.svg)", "",
             "![Go versus Rust construction time for Tree and Map at four sizes, mixed keys](benchmarks/build-arm64.svg)", "",
             "Memory is whole-process peak RSS, including the Go runtime, GC headroom and read caches. "
             "Construction inserts every entry into an empty container.", "",
             "<details>", "<summary>Memory and build measurements, mixed keys — each cell is Go / Rust</summary>", ""]
    text += memory_table("mixed")
    text += ["", "</details>", ""]
    for pattern, heading, suffix in patterns:
        if pattern == "mixed":
            continue
        name = heading.lower()
        text += [f"### {heading}", "",
                 f"{PATTERN_NOTES[pattern][0].upper()}{PATTERN_NOTES[pattern][1:]}.", "",
                 f"![Change in Go operation time versus Rust, {name}](benchmarks/cpu{suffix}-arm64.svg)", "",
                 f"![Go versus Rust construction time, {name}](benchmarks/build{suffix}-arm64.svg)", "",
                 f"<details>", f"<summary>Exact timings, memory and build measurements, {name} — Go / Rust</summary>", ""]
        text += timing_tables(pattern)
        text += [f"![Go versus Rust peak RSS, {name}](benchmarks/memory{suffix}-arm64.svg)", ""]
        text += memory_table(pattern)
        text += ["", "</details>", ""]
    text += [f"Small gaps are within run-to-run noise. The [raw report]({report_link}) adds Go's built-in `map` and `sync.Map`; "
             "amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).", ""]
    readme = ROOT / "README.md"
    old = readme.read_text()
    start = old.index("## Benchmarks")
    end = old.index("## Reproducing the benchmarks", start)
    updated = old[:start] + "\n".join(text) + "\n" + old[end:]
    updated = re.sub(r"use source checkpoint `[0-9a-f]+`", f"use source checkpoint `{go_short}`", updated)
    readme.write_text(updated)


if __name__ == "__main__":
    main()
