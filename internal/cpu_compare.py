"""Run each backend in a fresh process and validate matched checksums."""
import argparse
import json
import math
import re
import subprocess
from pathlib import Path


MASK64 = (1 << 64) - 1


def mix(value):
    value = ((value ^ (value >> 30)) * 0xbf58476d1ce4e5b9) & MASK64
    value = ((value ^ (value >> 27)) * 0x94d049bb133111eb) & MASK64
    return value ^ (value >> 31)


def key_of(x, size, pattern):
    """The key at index x, matching keyOf in cmd/compare."""
    miss = 0
    if x >= size:
        x, miss = x - size, 1
    if pattern == "sequential":
        return x << 1 | miss
    if pattern == "clustered":
        return mix(x >> 10) & ~((1 << 20) - 1) & MASK64 | (x & 1023) << 1 | miss
    return mix(x + miss*size)


def replacement_checksum(size, query_count, rounds, pattern):
    """Derive full previous-value sums, including repeated queries in a round."""
    total = delta = unique_delta = 0
    seen = set()
    for i in range(query_count):
        key = key_of(mix(i + 17) % size, size, pattern)
        total += key
        change = 1 if (key & 1) == 0 else -1  # (key ^ 1) - key
        delta += change
        if key not in seen:
            seen.add(key)
            unique_delta += change
    # For a key occurring c times per round, round zero returns key c times.
    # Each later round first returns the previous round's value, then c-1
    # copies of its own value. Thus the flipped value is returned
    # floor((rounds-1)/2) + (c-1)*floor(rounds/2) times per distinct key.
    repeated_delta = delta - unique_delta
    flipped_delta = (rounds // 2) * repeated_delta + ((rounds - 1) // 2) * unique_delta
    return (rounds * total + flipped_delta) & MASK64


SCALE_MIXES = ("read", "r90", "r50", "replace", "churn", "scan")
SCALE_LINE = re.compile(r"scale_(\w+)_w(\d+) ([\d.]+) ns/op checksum=(\d+) state=(\d+) count=(\d+)")
FIFTY_MILLION = 50_000_000


def parse_scale_line(line):
    """Parse one core-scaling row: scale_{mix}_w{workers} and its checks."""
    match = SCALE_LINE.fullmatch(line)
    if not match:
        raise RuntimeError(f"unexpected scaling output: {line}")
    mix_name, workers, elapsed, checksum, state, count = match.groups()
    return {"mix": mix_name, "workers": int(workers), "ns_per_op": float(elapsed),
            "checksum": int(checksum), "state": int(state), "count": int(count)}


def scale_cells(sizes, patterns, mixes, workers, fifty=False):
    """Every (size, pattern, mix, workers) cell; 50M entries run reads only."""
    cells = [(size, pattern, mix_name, w) for size in sizes for pattern in patterns for mix_name in mixes for w in workers]
    if fifty and "read" in mixes:
        cells += [(FIFTY_MILLION, pattern, "read", w) for pattern in patterns for w in workers]
    return cells


def scale_backends(backends, repeat, index):
    """Rotate the backend order by repeat and cell, so neither always runs first."""
    rotation = (repeat + index) % len(backends)
    return backends[rotation:] + backends[:rotation]


def scale_command(language, binary, size, pattern, mix_name, workers, ops, queries):
    """The command line for one fresh-process scaling cell."""
    if language == "rust":
        return [str(binary), str(size), "1", str(workers), str(queries), "map", f"scale={mix_name}", f"ops={ops}", f"keys={pattern}"]
    return [str(binary), "-kind", "map", "-size", str(size), "-threads", str(workers), "-queries", str(queries), "-scale", mix_name, "-ops", str(ops), f"-keys={pattern}"]


def check_scale_agreement(expected, cell, result, language):
    """Every backend and repeat of a cell must produce the same checksums."""
    observed = (result["checksum"], result["state"], result["count"])
    want = expected.setdefault(cell, observed)
    if want != observed:
        raise RuntimeError(f"scaling mismatch for {language} {cell}: got {observed}, want {want}")


def median(values):
    ordered = sorted(values)
    middle = len(ordered) // 2
    if len(ordered) % 2:
        return ordered[middle]
    return (ordered[middle - 1] + ordered[middle]) / 2


def summarize_scaling(runs):
    """Median Mops/s per backend and cell, speedup over one worker, and Go/Rust.

    Each run is a dict with language, size, keys, mix, workers and ns_per_op;
    ns_per_op is wall time over all workers' operations, so Mops/s is 1000/ns.
    """
    samples = {}
    for run in runs:
        samples.setdefault((run["size"], run["keys"], run["mix"], run["workers"], run["language"]), []).append(run["ns_per_op"])
    mops = {key: 1000 / median(values) for key, values in samples.items()}
    rows = []
    for size, keys, mix_name, workers in sorted({key[:4] for key in mops}, key=lambda c: (c[0], c[1], SCALE_MIXES.index(c[2]) if c[2] in SCALE_MIXES else len(SCALE_MIXES), c[2], c[3])):
        row = {"size": size, "keys": keys, "mix": mix_name, "workers": workers}
        for language in ("go", "rust"):
            value = mops.get((size, keys, mix_name, workers, language))
            if value is None:
                continue
            base = mops.get((size, keys, mix_name, 1, language))
            row[f"{language}_mops"] = round(value, 3)
            row[f"{language}_speedup"] = round(value / base, 3) if base else None
        if "go_mops" in row and "rust_mops" in row:
            row["go_over_rust"] = round(row["go_mops"] / row["rust_mops"], 3)
        rows.append(row)
    return rows


def run_scaling(args):
    """Run each scaling cell in a fresh process per backend and repeat."""
    expected = {}
    runs = []
    backends = [("go", args.go.resolve()), ("rust", args.rust.resolve())]
    cells = scale_cells(args.sizes, args.patterns, args.mixes, args.workers, args.fifty)
    for repeat in range(args.repeat):
        for index, (size, pattern, mix_name, workers) in enumerate(cells):
            ops = args.scan_ops if mix_name == "scan" else args.ops
            for language, binary in scale_backends(backends, repeat, index):
                command = scale_command(language, binary, size, pattern, mix_name, workers, ops, args.queries)
                lines = subprocess.check_output(command, text=True).splitlines()
                if len(lines) != 2:
                    raise RuntimeError(f"unexpected scaling output: {lines}")
                result = parse_scale_line(lines[1])
                if result["mix"] != mix_name or result["workers"] != workers or result["count"] != size:
                    raise RuntimeError(f"scaling row does not match its cell: {lines[1]}")
                check_scale_agreement(expected, (size, pattern, mix_name, workers, ops, args.queries), result, language)
                run = {"language": language, "repeat": repeat, "size": size, "keys": pattern, "mix": mix_name, "workers": workers, "ops": ops, "queries": args.queries, "build": lines[0], **result}
                runs.append(run)
                print(json.dumps(run), flush=True)
    print(json.dumps({"scaling": summarize_scaling(runs)}), flush=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", required=True, type=Path)
    parser.add_argument("--rust", required=True, type=Path)
    parser.add_argument("--baseline", type=Path)
    parser.add_argument("--size", type=int, default=5_000_000)
    parser.add_argument("--queries", type=int, default=1_048_576)
    parser.add_argument("--rounds", type=int, default=4)
    parser.add_argument("--threads", type=int, default=8)
    parser.add_argument("--repeat", type=int, default=3)
    parser.add_argument("--kinds", nargs="+", choices=["tree", "map"], default=["tree", "map"])
    parser.add_argument("--standard", action="store_true")
    parser.add_argument("--mutations", action="store_true")
    parser.add_argument("--distinct", action="store_true")
    parser.add_argument("--keys", choices=["mixed", "sequential", "clustered"], default="mixed")
    parser.add_argument("--text", action="store_true", help="run the string-keyed cells instead: each key as user: and 16 hex digits")
    scaling = parser.add_argument_group("core scaling (Map only, one cell per process)")
    scaling.add_argument("--scaling", action="store_true", help="run the core-scaling matrix instead of the rows above")
    scaling.add_argument("--workers", nargs="+", type=int, default=[1, 2, 4, 8, 12])
    scaling.add_argument("--sizes", nargs="+", type=int, default=[262_144, 5_000_000])
    scaling.add_argument("--patterns", nargs="+", choices=["mixed", "sequential", "clustered"], default=["mixed", "sequential", "clustered"])
    scaling.add_argument("--mixes", nargs="+", choices=SCALE_MIXES, default=list(SCALE_MIXES))
    scaling.add_argument("--ops", type=int, default=1 << 20, help="operations per worker")
    scaling.add_argument("--scan-ops", type=int, default=1 << 16, help="operations per worker in the scan mix")
    scaling.add_argument("--fifty", action="store_true", help="add read-only cells at 50M entries (slow to build)")
    args = parser.parse_args()
    if args.scaling:
        if args.baseline:
            parser.error("--scaling compares the current Go and Rust drivers only")
        if args.repeat < 1 or args.ops < 1 or args.scan_ops < 1 or min(args.workers) < 1 or min(args.sizes) < max(args.workers):
            parser.error("repeat, ops and workers must be positive; every size needs a key per worker")
        if args.queries < 8 or args.queries & (args.queries - 1):
            parser.error("queries must be a power of two, at least eight")
        run_scaling(args)
        return
    if args.size < 1 or args.rounds < 1 or args.repeat < 1 or args.threads < 0:
        parser.error("size, rounds and repeat must be positive; threads nonnegative")
    if args.queries < 8 or args.queries & (args.queries - 1):
        parser.error("queries must be a power of two, at least eight")
    if args.baseline and args.keys != "mixed":
        parser.error("the baseline harness predates key patterns; use it with mixed keys")
    if args.text and (args.baseline or args.standard or args.distinct):
        parser.error("--text runs Tree and Map cells only, without a baseline or distinct churn")
    checksums = {}
    if args.mutations:
        checksums["replace"] = replacement_checksum(args.size, args.queries, args.rounds, args.keys)
    distinct_queries = min(args.queries, args.size)
    distinct_rounds = args.rounds * (args.queries // distinct_queries)
    if args.distinct:
        step = 0x9e3779b97f4a7c15 % args.size
        while math.gcd(step,args.size) != 1:
            step += 1
        indices = [(17 + i*step) % args.size for i in range(distinct_queries)]
        original = sum(key_of(i, args.size, args.keys) for i in indices)
        alternate = sum(key_of(i + args.size, args.size, args.keys) for i in indices)
        del indices
        checksums["churn_new_pair"] = (original*((distinct_rounds+1)//2) + alternate*(distinct_rounds//2)) & MASK64
    kinds = list(dict.fromkeys(args.kinds)) + (["builtin", "syncmap"] if args.standard else [])
    for repeat in range(args.repeat):
        for kind in kinds:
            backends = [("go", args.go)]
            if args.baseline and kind in ("tree", "map"):
                backends.insert(0, ("baseline", args.baseline))
            if kind in ("tree", "map"):
                backends.append(("rust", args.rust))
            rotation = repeat % len(backends)
            backends = backends[rotation:] + backends[:rotation]
            for language, binary in backends:
                if language == "rust":
                    command = [str(binary.resolve()), str(args.size), str(args.rounds), str(args.threads), str(args.queries), kind]
                else:
                    command = [str(binary.resolve()), "-kind", kind, "-size", str(args.size), "-rounds", str(args.rounds), "-threads", str(args.threads), "-queries", str(args.queries)]
                if args.mutations:
                    command.append("mutations" if language == "rust" else "-mutations")
                if args.distinct:
                    command.append("distinct" if language == "rust" else "-distinct")
                if args.keys != "mixed":
                    command.append(f"keys={args.keys}" if language == "rust" else f"-keys={args.keys}")
                if args.text:
                    command.append("text" if language == "rust" else "-text")
                output = subprocess.check_output(command, text=True)
                lines = output.splitlines()
                cases = []
                for line in lines[1:]:
                    match = re.fullmatch(r"(\w+) ([\d.]+) ns/op checksum=(\d+)", line)
                    if not match:
                        raise RuntimeError(f"unexpected benchmark output: {line}")
                    name, elapsed, checksum = match.groups()
                    equivalent = re.sub(r"^(tree_batch8|map_batch8|tree|map|builtin|syncmap|rwmap)_", "", name)
                    checksum = int(checksum)
                    expected = checksums.setdefault(equivalent, checksum)
                    if expected != checksum:
                        raise RuntimeError(f"checksum mismatch for {language} {name}: got {checksum}, want {expected}")
                    cases.append({"name": name, "ns_per_op": float(elapsed), "checksum": checksum})
                if not cases:
                    raise RuntimeError("benchmark returned no operations")
                print(json.dumps({"language": language, "kind": kind, "repeat": repeat, "entries": args.size, "queries": args.queries, "rounds": args.rounds, "threads": args.threads, "mutations": args.mutations, "distinct": args.distinct, "keys": args.keys, "text": args.text, "distinct_queries": distinct_queries, "distinct_rounds": distinct_rounds, "build": lines[0], "cases": cases}), flush=True)


if __name__ == "__main__":
    main()
