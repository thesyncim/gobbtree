"""Render the checked-in Go/Rust report as README cards and tables.

The cards are upstream's (bbtree's design/assets/readme-race.svg): one
workload, a row per container, each a ball whose one-way trip takes 0.6 s
times gobbtree's throughput over its own. Run from the repository root.
"""
import argparse
import json
import re
from collections import defaultdict
from html import escape
from pathlib import Path
from statistics import median

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
# The cards' workloads: 5M mixed uint64 keys, as upstream's 10M UUIDs.
CARD_SIZE = 5000000


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
    # Every case of every row, Go's built-in maps' among them, by name.
    cases_by_name = defaultdict(list)
    for row in report["cpu"]:
        if not row.get("text"):
            for case in row["cases"]:
                cases_by_name[row.get("keys", "mixed"), row["entries"], row["language"], case["name"]].append(case["ns_per_op"])
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

    def mops(language, name):
        return 1000/median(cases_by_name["mixed", CARD_SIZE, language, name])

    hardware = "Apple M4 Max"
    concurrent = race_card(
        "race-concurrent-arm64", "concurrent get, 4 threads", f"5M uint64 keys · {hardware} · M ops/s",
        [("gobbtree Map", mops("go", "map_parallel4_hit")), ("bbtree (Rust)", mops("rust", "map_parallel4_hit"))],
        [("sync.Map (unordered)", mops("go", "syncmap_parallel4_hit"))],
        f"Concurrent get on 5M mixed uint64 keys, 4 threads, {hardware}, in million operations per second",
        f"{report_link}: mixed, 5M entries, map_parallel4_hit and syncmap_parallel4_hit, medians of 3 runs.")
    single = race_card(
        "race-single-arm64", "get, single thread", f"5M uint64 keys · {hardware} · M ops/s",
        [("gobbtree Tree", mops("go", "tree_hit")), ("bbtree (Rust)", mops("rust", "tree_hit"))],
        [("Go map (unordered)", mops("go", "builtin_hit"))],
        f"Get on 5M mixed uint64 keys, one thread, {hardware}, in million operations per second",
        f"{report_link}: mixed, 5M entries, tree_hit and builtin_hit, medians of 3 runs.")
    for path in (concurrent, single):
        if args.preview_dir:
            args.preview_dir.mkdir(parents=True, exist_ok=True)
            (args.preview_dir / path.name).write_text(path.read_text())

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
        rows = ["| Container | Entries | Peak, MB | After reads, MB | Final, MB | Build, s |",
                "|---|---:|---:|---:|---:|---:|"]
        for kind in ("tree", "map"):
            for size, label in zip(SIZES, LABELS):
                cells = []
                for metric in ("peak", "read", "settled", "build"):
                    precision = 4 if metric == "build" else 2
                    cells.append(" / ".join(f"{memory(pattern, size, kind, language, metric):,.{precision}f}" for language in ("go", "rust")))
                rows.append(f"| {kind.title()} | {label} | " + " | ".join(cells) + " |")
        return rows

    described = [f"{heading.lower()} ({PATTERN_NOTES[pattern]})" for pattern, heading, _ in patterns]
    described = ", ".join(described[:-1]) + " and " + described[-1] if len(described) > 1 else described[0]
    text = ["## Benchmarks", "",
            "![gobbtree's Tree against Rust bbtree's and Go's map, get throughput on one thread](benchmarks/race-single-arm64.svg)", "",
            f"gobbtree's `Tree` and `Map` against Rust bbtree's `unsync::BBTreeMap` and `BBTreeMap` on an {hardware}: "
            f"Go {go_version} with `GOEXPERIMENT=simd` at [`{go_short}`](https://github.com/thesyncim/gobbtree/commit/{go_source}), "
            f"Rust {rust_version} with native CPU tuning, full LTO and mimalloc at "
            f"[`{rust_short}`](https://github.com/XiangpengHao/bbtree/tree/{rust_source}). "
            "Each cell is the median of three fresh-process runs over identical keys, with checked results.", "",
            # This summary describes the checked-in report; revise it with a new one.
            "gobbtree is faster in every `uint64` timing below. Building `uint64` maps is faster in Rust at 4K entries, "
            "and 1.4× faster for the sequential `Tree` at 262K and 5M; Go builds clustered keys 1.5–2.3× faster from 262K up, "
            "and the other builds are within 20% either way. "
            "Go's process peaks 1.4–3.6 MB higher at 4K and 262K entries, and lower from 5M up. "
            "With string keys Rust still wins 14 of 22 cells, among them every build and most of the `Map`'s.", "",
            f"The tables cover `uint64` keys and values from 4K to 50M entries, with {described}. "
            "Batch cells are per key, churn cells per delete-and-insert pair, and parallel cells use four workers.", ""]
    for pattern, heading, _ in patterns:
        text += ["<details>", f"<summary>Times, {heading.lower()} — Go / Rust, ns/op</summary>", ""]
        text += timing_tables(pattern)
        text += ["</details>", ""]
    text += ["<details>", "<summary>Memory and build — Go / Rust</summary>", "",
             "Whole-process RSS, the Go runtime included, under Go's default GC settings: at its peak, after a full read pass, "
             "and at the end, after a forced GC in Go. Build is the time to insert every entry, one at a time.", ""]
    for pattern, heading, _ in patterns:
        text += [f"**{heading}**", ""] + memory_table(pattern) + [""]
    text += ["</details>", ""]
    if texts:
        text += string_key_section(texts)
    text += ["Small gaps are inconclusive: a macOS system service used one or two cores throughout the run. "
             f"The [raw report]({report_link}) holds every run, with Go's built-in `map` and `sync.Map` at 4K to 5M mixed keys; "
             "amd64 results are [CI artifacts](https://github.com/thesyncim/gobbtree/actions/workflows/amd64-simd.yml).", ""]
    readme = ROOT / "README.md"
    old = readme.read_text()
    start = old.index("## Benchmarks")
    end = old.index("## Reproducing the benchmarks", start)
    updated = old[:start] + "\n".join(text) + "\n" + old[end:]
    # The concurrent card leads, after the introduction's list, as upstream's.
    hero = "![gobbtree's Map against Rust bbtree's and sync.Map, get throughput at 4 threads](benchmarks/race-concurrent-arm64.svg)\n\n"
    updated = re.sub(r"!\[[^\]]*\]\(benchmarks/race-concurrent-arm64\.svg\)\n\n", "", updated)
    usage = updated.index("## Usage")
    updated = updated[:usage] + hero + updated[usage:]
    updated = re.sub(r"source checkpoint `[0-9a-f]+`", f"source checkpoint `{go_short}`", updated)
    readme.write_text(updated)


def race_card(name, title, subtitle, rows, baselines, label, source):
    """Write benchmarks/<name>.svg as upstream's race card: rows of
    (container, M ops/s), the first gobbtree's, highlighted; baselines,
    unordered, below a dashed rule. Returns the path."""
    ours = rows[0][1]

    def value(v):
        return f"{v:.0f}" if v >= 100 else f"{v:.3g}"

    def width(text):
        return round(len(text) * 10.83)

    def ball(y, v, style):
        return (f'  <circle class="ball" cx="269" cy="{y - 8}" r="11" {style} '
                f'style="animation-duration: {0.6 * ours / v:.2f}s"/>')

    y = 106
    body = []
    for i, (container, v) in enumerate(rows):
        if i == 0:
            body += [f'  <rect class="highlight" x="22" y="{y - 20}" width="{width(container) + 12}" height="26" rx="4"/>',
                     f'  <rect class="highlight" x="{680 - width(value(v)) - 16}" y="{y - 20}" width="{width(value(v)) + 16}" height="26" rx="4"/>',
                     f'  <text class="ours" x="28" y="{y}">{escape(container)}</text>',
                     f'  <line class="track" x1="258" y1="{y - 8}" x2="590" y2="{y - 8}"/>',
                     ball(y, v, 'fill="#ffd60a" stroke="#18181b" stroke-width="2.5"'),
                     f'  <text class="ours value" x="672" y="{y}">{value(v)}</text>', ""]
        else:
            body += [f'  <text x="28" y="{y}">{escape(container)}</text>',
                     f'  <line class="track" x1="258" y1="{y - 8}" x2="590" y2="{y - 8}"/>',
                     ball(y, v, 'fill="#3f3f46"'),
                     f'  <text class="value" x="672" y="{y}">{value(v)}</text>', ""]
        y += 40
    if baselines:
        rule = y - 40 + 16
        body += [f'  <line class="track dashed" x1="28" y1="{rule}" x2="672" y2="{rule}" stroke-width="1.5"/>', ""]
        y = rule + 32
        for container, v in baselines:
            body += [f'  <text class="faint" x="28" y="{y}">{escape(container)}</text>',
                     f'  <line class="track dashed" x1="258" y1="{y - 8}" x2="590" y2="{y - 8}"/>',
                     ball(y, v, 'fill="none" stroke="#71717a" stroke-width="2.5"'),
                     f'  <text class="faint value" x="672" y="{y}">{value(v)}</text>', ""]
            y += 40
    height = y - 40 + 22
    aria = (f"{label}: " + ", ".join(f"{c} {value(v)}" for c, v in rows) +
            "".join(f"; {c.replace(' (unordered)', '')}, unordered, {value(v)}" for c, v in baselines) + ".")
    svg = [f'<svg xmlns="http://www.w3.org/2000/svg" width="700" height="{height}" viewBox="0 0 700 {height}" role="img" aria-label="{escape(aria)}">',
           f"  <!-- {escape(source)}",
           "       A ball's one-way trip takes 0.6 s × (gobbtree's throughput / its throughput). -->",
           "  <style>",
           "    text { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 18px; font-weight: 600; fill: #18181b; }",
           "    .title { font-weight: 800; }",
           "    .muted { fill: #52525b; }",
           "    .faint { fill: #71717a; }",
           "    .ours { font-weight: 800; }",
           "    .value { text-anchor: end; }",
           "    .highlight { fill: #ffd60a; }",
           "    .track { stroke: #a1a1aa; stroke-width: 3; stroke-linecap: round; }",
           "    .dashed { stroke-dasharray: 4 6; }",
           "    .ball { animation: bounce linear infinite alternate; }",
           "    @keyframes bounce { to { transform: translateX(310px); } }",
           "  </style>",
           f'  <rect x="0.5" y="0.5" width="699" height="{height - 1}" rx="12" fill="#fafafa" stroke="#d4d4d8"/>',
           "",
           f'  <text class="title" x="28" y="40">{escape(title)}</text>',
           f'  <text class="muted" x="28" y="66">{escape(subtitle)}</text>',
           ""] + body[:-1] + ["</svg>", ""]
    path = ROOT / "benchmarks" / f"{name}.svg"
    path.write_text("\n".join(svg))
    return path


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
            "Go `string` keys against Rust `String`s, each a mixed key written as `user:` and 16 hex digits, "
            "with `uint64` values. Rust's containers move in keys made before the timed build; Go's copy each "
            "key's bytes into their leaves. Rust replaces values with `update` and `get_mut`, and its churn "
            "reinserts the key its removal returned. In parentheses, Go's time against Rust's: negative is faster.", "",
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
