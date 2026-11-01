"""Each cell's change between two reports, Go's beside Rust's.

report_drift.py <old report> <new report>

Rust's code is pinned, so its change is the machine's; a Go cell that moves
well past it, or a cell that changes side, wants a control before a report
is published: the old report's driver, run beside the new one.
"""
import json
import sys
from collections import defaultdict
from statistics import median


def cells(report):
    values = defaultdict(list)
    for row in report["cpu"]:
        pattern = "text" if row.get("text") else row.get("keys", "mixed")
        for case in row["cases"]:
            values[pattern, row["entries"], row["language"], case["name"]].append(case["ns_per_op"])
    for row in report["rss"]:
        key = (row.get("keys", "mixed"), row["stages"][0]["entries"], row["language"])
        values[key + (row["kind"] + "_build",)].append(row["stages"][0]["build_ns"])
        values[key + (row["kind"] + "_peak",)].append(row["peak_rss_bytes"])
    for row in report.get("store", []):
        for field in ("insert_ns", "get_ns", "scan_ns", "threads8_insert_ns", "threads8_get_ns"):
            values["store", row["entries"], row["language"], field].append(row[field])
    return {k: median(v) for k, v in values.items()}


def main():
    old, new = (cells(json.load(open(p))) for p in sys.argv[1:3])
    rows, flips = [], 0
    for key in sorted(new):
        rust = key[:2] + ("rust",) + key[3:]
        if key[2] != "go" or key not in old or rust not in old or rust not in new:
            continue
        go_change, rust_change = 100 * (new[key] / old[key] - 1), 100 * (new[rust] / old[rust] - 1)
        if (old[key] < old[rust]) != (new[key] < new[rust]):
            flips += 1
            print("changed side:", key[:2] + key[3:], f"go {old[key]:.4g} -> {new[key]:.4g}, rust {old[rust]:.4g} -> {new[rust]:.4g}")
        rows.append((go_change - rust_change, go_change, rust_change, key[:2] + key[3:]))
    rows.sort()
    print("Go's change past Rust's, the widest either way:")
    for drift, go_change, rust_change, key in rows[:5] + rows[-10:]:
        print(f"  {drift:+6.1f}  go {go_change:+6.1f}%  rust {rust_change:+6.1f}%  {key}")
    print(f"{len(rows)} cells, {flips} changed side")


if __name__ == "__main__":
    main()
