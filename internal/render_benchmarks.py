"""Render the checked-in Go/Rust report as SVG charts and README tables.

Requires matplotlib; run from the repository root.
"""
import argparse
import json
import math
import re
from collections import defaultdict
from pathlib import Path
from statistics import median

import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from matplotlib.patches import Rectangle

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
    cpu, rss, texts = defaultdict(list), defaultdict(list), defaultdict(list)
    for row in report["cpu"]:
        if row.get("text"):
            for case in row["cases"]:
                texts[row["entries"], row["language"], case["name"]].append(case["ns_per_op"])
            continue
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

    # One figure, a column per container: a row per workload, each
    # measurement Go's speed relative to Rust's on a log scale, 1x parity.
    def ratio_rows(kind):
        rows = [("head", "uint64 keys", [])]
        for case, title in cases[kind]:
            rows.append(("row", title, [(time(p, size, kind, "rust", case)/time(p, size, kind, "go", case), i)
                                        for p, _, _ in patterns for i, size in enumerate(SIZES)]))
        for metric, title in (("build", "Build"), ("peak", "Peak memory")):
            rows.append(("row", title, [(memory(p, size, kind, "rust", metric)/memory(p, size, kind, "go", metric), i)
                                        for p, _, _ in patterns for i, size in enumerate(SIZES)]))
        if texts:
            rows.append(("head", "string keys", []))
            for case, title in TEXT_CASES:
                name = f"text_{kind}_{case}"
                points = [(median(texts[s, "rust", name])/median(texts[s, "go", name]), SIZES.index(s))
                          for s in SIZES if (s, "go", name) in texts and (s, "rust", name) in texts]
                if points:
                    rows.append(("row", title.replace(", per insert", ""), points))
        return rows

    columns = [(kind, ratio_rows(kind)) for kind in ("tree", "map")]
    step, head_step, top, bottom = 26, 34, 128, 104
    body = max(sum(head_step if r[0] == "head" else step for r in rows) for _, rows in columns)
    height = top + body + bottom
    width = 1240
    fig = plt.figure(figsize=(width/72, height/72))
    ax = fig.add_axes([0, 0, 1, 1])
    ax.set(xlim=(0, width), ylim=(height, 0))
    ax.axis("off")
    low, high = math.log(1/4), math.log(4)
    shades = ["#9fd8c4", "#5fb39a", "#24806b", "#0b4f42"]
    ax.text(30, 34, "Go vs Rust", fontsize=24, weight="bold", va="center")
    ax.text(30, 66, "Go's speed relative to Rust: one dot per measurement, right of 1× means Go is faster; "
            "memory compares peak RSS", fontsize=14, color=MUTED, va="center")
    for i, (name, shade) in enumerate(zip(LABELS, shades)):
        ax.plot([36 + 120*i], [98], "o", color=shade, markersize=8)
        ax.text(50 + 120*i, 98, name + " entries", fontsize=13, va="center")
    ax.plot([532, 532], [90, 106], color=INK, linewidth=2.2)
    ax.text(546, 98, "geometric mean", fontsize=13, va="center")
    for c, (kind, rows) in enumerate(columns):
        x0 = 30 + c*610
        left, right = x0 + 200, x0 + 520

        def x_of(ratio):
            return left + (math.log(min(max(ratio, 1/4), 4)) - low)/(high - low)*(right - left)

        y = top
        ax.text(x0, y + 2, ("Tree · single owner" if kind == "tree" else "Map · concurrent").upper(),
                fontsize=14, weight="bold", va="center")
        body_top = y + 16
        y = body_top
        for row, title, points in rows:
            if row == "head":
                y += head_step
                ax.text(x0, y - 10, title, fontsize=12, weight="bold", color=MUTED, va="center")
                continue
            y += step
            cy = y - step/2
            ratios = [r for r, _ in points]
            mean = math.exp(sum(math.log(r) for r in ratios)/len(ratios))
            ax.text(x0, cy, title, fontsize=13, va="center")
            ax.plot([x_of(min(ratios)), x_of(max(ratios))], [cy, cy], color="#cfd8e3", linewidth=6,
                    solid_capstyle="round", zorder=1)
            for r, i in points:
                ax.plot([x_of(r)], [cy], "o", color=shades[i], markersize=5.5, zorder=2, alpha=.9)
            ax.plot([x_of(mean)]*2, [cy - 8, cy + 8], color=INK, linewidth=2.2, zorder=3)
            ink = "#095d48" if mean > 1.05 else "#922f12" if mean < 1/1.05 else "#405469"
            ax.text(right + 14, cy, f"{mean:.2f}×", fontsize=13, weight="bold", color=ink, va="center")
        body_bottom = y + 4
        ax.add_patch(Rectangle((left, body_top), x_of(1) - left, body_bottom - body_top,
                               facecolor="#fbeee8", edgecolor="none", zorder=0))
        for tick in (1/4, 1/2, 1, 2, 4):
            x = x_of(tick)
            ax.plot([x, x], [body_top, body_bottom], color=INK if tick == 1 else GRID,
                    linewidth=1.2 if tick == 1 else .8, zorder=.5)
            label = "1×" if tick == 1 else f"{tick:g}×" if tick > 1 else f"1/{1/tick:g}×"
            ax.text(x, body_bottom + 16, label, fontsize=12, color=MUTED, ha="center", va="center")
        ax.text(left, body_bottom + 38, "◀ Rust faster", fontsize=12, color="#922f12", va="center")
        ax.text(right, body_bottom + 38, "Go faster ▶", fontsize=12, color="#095d48", va="center", ha="right")
    ax.text(30, height - 12, f"Apple M4 Max · medians of 3 runs · Go {go_version} / {go_short} · Rust {rust_version} / {rust_short} · "
            "uint64 rows: 4K–50M entries, mixed, sequential and clustered keys · string rows: 262K and 5M",
            fontsize=11, color=MUTED, va="center")
    save(fig, "summary-arm64", "Go's speed relative to Rust per workload, Tree and Map, uint64 and string keys")

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
            f"on an Apple M4 Max, from [Go `{go_short}`](https://github.com/thesyncim/gobbtree/commit/{go_source}) and "
            f"[Rust `{rust_short}`](https://github.com/XiangpengHao/bbtree/tree/{rust_source}): medians of three fresh-process runs "
            "over identical datasets, with checked results.", "",
            "![Go's speed relative to Rust for each workload, Tree and Map, uint64 and string keys](benchmarks/summary-arm64.svg)", "",
            "Each dot is one measurement; `uint64` rows cover 4K to 50M entries with " +
            ", ".join(f"{heading.lower()} ({PATTERN_NOTES[pattern]})" for pattern, heading, _ in patterns) +
            ". Batches are per key, churn is one deletion and one insertion, and parallel rows use four workers. "
            "Memory is whole-process peak RSS, including the Go runtime and GC headroom.", ""]
    for pattern, heading, _ in patterns:
        text += ["<details>", f"<summary>Times, {heading.lower()} — Go / Rust, ns/op</summary>", ""]
        text += timing_tables(pattern)
        text += ["</details>", ""]
    text += ["<details>", "<summary>Memory and build — Go / Rust</summary>", ""]
    for pattern, heading, _ in patterns:
        text += [f"**{heading}**", ""] + memory_table(pattern) + [""]
    text += ["</details>", ""]
    if texts:
        text += string_key_section(texts)
    text += [f"Small gaps are within run-to-run noise. The [raw report]({report_link}) adds Go's built-in `map` and `sync.Map`; "
             "amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).", ""]
    readme = ROOT / "README.md"
    old = readme.read_text()
    start = old.index("## Benchmarks")
    end = old.index("## Reproducing the benchmarks", start)
    updated = old[:start] + "\n".join(text) + "\n" + old[end:]
    updated = re.sub(r"source checkpoint `[0-9a-f]+`", f"source checkpoint `{go_short}`", updated)
    readme.write_text(updated)


TEXT_CASES = [
    ("build", "Build, per insert"), ("hit", "Read hit"), ("miss", "Read miss"),
    ("replace", "Replace value"), ("churn_pair", "Delete + reinsert same key"),
    ("parallel4_hit", "4 workers · reads"),
]


def string_key_section(texts):
    """The string-keyed cells: Go / Rust ns/op and Go's change, per size."""
    sizes = sorted({entries for entries, _, _ in texts})
    labels = {s: l for s, l in zip(SIZES, LABELS)}
    rows = ["<details>", "<summary>String keys — Go / Rust, ns/op</summary>", "",
            "`string` keys against Rust's `String`, each mixed key written as `user:` and 16 hex digits, "
            "with `uint64` values. Rust's containers move in keys made beforehand; Go's copy each key's bytes "
            "into their leaves, in buffers the collector doesn't scan. Rust replaces values with `update` and "
            "`get_mut`, and its churn inserts back the key its removal returned.", "",
            "| Workload | " + " | ".join(f"{labels.get(s, s)}" for s in sizes) + " |",
            "|---|" + "---:|" * len(sizes)]
    for kind in ("tree", "map"):
        for case, label in TEXT_CASES:
            name = f"text_{kind}_{case}"
            if not any((s, "go", name) in texts for s in sizes):
                continue
            cells = []
            for size in sizes:
                go, rust = texts.get((size, "go", name)), texts.get((size, "rust", name))
                if not go or not rust:
                    cells.append("—")
                    continue
                g, r = median(go), median(rust)
                cells.append(f"{g:.1f} / {r:.1f} ({100*(g/r-1):+.0f}%)")
            rows.append(f"| {kind.title()} · {label} | " + " | ".join(cells) + " |")
    return rows + ["", "</details>", ""]


if __name__ == "__main__":
    main()
