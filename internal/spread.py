"""The widest spread among one backend's runs in a segment's records.

spread.py <records.jsonl> <limit>

Each run is scored by its serial timings summed (a CPU record's cases but
the parallel ones, a store's insert, get and churn, a memory record's
build), and the spread is a backend's slowest run over its fastest. It
prints the widest, and exits 1 past the limit: another process that takes
the machine's memory bandwidth for a minute or two slows whole processes of
either language twofold.
"""
import json
import sys
from collections import defaultdict


def runs(path):
    scores = defaultdict(list)
    for line in open(path):
        if not line.startswith("{") or '"language"' not in line:
            continue
        row = json.loads(line)
        if "cases" in row:
            total = sum(c["ns_per_op"] for c in row["cases"] if "parallel" not in c["name"])
            scores[row["language"], row.get("kind"), row["entries"]].append(total)
        elif "stages" in row:
            scores[row["language"], row["kind"], row["stages"][0]["entries"]].append(row["stages"][0]["build_ns"])
        else:
            scores[row["language"], "store", row["entries"]].append(
                row["insert_ns"] + row["get_ns"] + row["churn"][-1]["ns_per_op"])
    return scores


def main():
    path, limit = sys.argv[1], float(sys.argv[2])
    scores = runs(path)
    if not scores:
        sys.exit(f"{path}: no records")
    worst, backend = max((max(v) / min(v), k) for k, v in scores.items())
    print(f"{path.rsplit('/', 1)[-1]}: widest spread {worst:.2f} at {backend}")
    sys.exit(1 if worst > limit else 0)


if __name__ == "__main__":
    main()
