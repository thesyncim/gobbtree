"""Compare stores: Go's storebench against upstream bbtree-store's examples/bench.rs.

Each run is a fresh process of either program, alternating, Go's twice: its
stores of Go's pages, and, as go-mapped, stores that map their own
(storebench -mapped). Each prints upstream's report, which this parses into
one JSON record a run.
"""
import argparse
import json
import re
import subprocess
from pathlib import Path

LINES = {
    "entries": r"^(\d+) entries of 20-byte keys and 32-byte values$",
    "one": r"^1 thread: +insert (\d+) ns, get (\d+) ns, scan ([\d.]+) ns an entry; "
           r"nodes ([\d.]+) \+ heap ([\d.]+) = [\d.]+ bytes an entry$",
    "churn": r"^churn round (\d+): heap ([\d.]+) bytes a live record byte, (\d+) ns an op$",
    "threads": r"^8 threads: insert (\d+) ns, get (\d+) ns an entry, wall time$",
    "long": r"^(\d+) entries of 4000-byte values: insert (\d+) ns, get (\d+) ns; heap ([\d.]+) bytes a value byte$",
}


def parse(output, entries):
    record, churn = {}, []
    for line in output.splitlines():
        for name, pattern in LINES.items():
            m = re.match(pattern, line.strip())
            if not m:
                continue
            if name == "entries":
                record["entries"] = int(m[1])
            elif name == "one":
                record.update(insert_ns=int(m[1]), get_ns=int(m[2]), scan_ns=float(m[3]),
                              node_bytes=float(m[4]), heap_bytes=float(m[5]))
            elif name == "churn":
                churn.append({"round": int(m[1]), "heap_per_live_byte": float(m[2]), "ns_per_op": int(m[3])})
            elif name == "threads":
                record.update(threads8_insert_ns=int(m[1]), threads8_get_ns=int(m[2]))
            else:
                record.update(long_entries=int(m[1]), long_insert_ns=int(m[2]), long_get_ns=int(m[3]),
                              long_heap_per_value_byte=float(m[4]))
    record["churn"] = churn
    want = {"entries", "insert_ns", "threads8_get_ns", "long_heap_per_value_byte"}
    if not want <= record.keys() or record["entries"] != entries or len(churn) != 4:
        raise RuntimeError(f"unparsed report:\n{output}")
    return record


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", required=True, type=Path)
    parser.add_argument("--rust", required=True, type=Path)
    parser.add_argument("--size", type=int, default=1_000_000)
    parser.add_argument("--repeat", type=int, default=3)
    args = parser.parse_args()
    if args.size < 16 or args.repeat < 1:
        parser.error("size must be at least 16 and repeat positive")
    for repeat in range(args.repeat):
        commands = [("go", [args.go]), ("go-mapped", [args.go, "-mapped"]), ("rust", [args.rust])]
        if repeat % 2:
            commands.reverse()
        for language, (program, *flags) in commands:
            output = subprocess.run([str(program.resolve()), *flags, str(args.size)], check=True,
                                    capture_output=True, text=True).stdout
            print(json.dumps({"language": language, "repeat": repeat, **parse(output, args.size)}), flush=True)


if __name__ == "__main__":
    main()
