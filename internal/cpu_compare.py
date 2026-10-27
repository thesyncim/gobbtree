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
    args = parser.parse_args()
    if args.size < 1 or args.rounds < 1 or args.repeat < 1 or args.threads < 0:
        parser.error("size, rounds and repeat must be positive; threads nonnegative")
    if args.queries < 8 or args.queries & (args.queries - 1):
        parser.error("queries must be a power of two, at least eight")
    if args.baseline and args.keys != "mixed":
        parser.error("the baseline harness predates key patterns; use it with mixed keys")
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
                print(json.dumps({"language": language, "kind": kind, "repeat": repeat, "entries": args.size, "queries": args.queries, "rounds": args.rounds, "threads": args.threads, "mutations": args.mutations, "distinct": args.distinct, "keys": args.keys, "distinct_queries": distinct_queries, "distinct_rounds": distinct_rounds, "build": lines[0], "cases": cases}), flush=True)


if __name__ == "__main__":
    main()
