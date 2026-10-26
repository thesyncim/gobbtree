"""Run each backend in a fresh process and validate matched checksums."""
import argparse
import json
import re
import subprocess
from pathlib import Path


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
    args = parser.parse_args()
    if args.size < 1 or args.rounds < 1 or args.repeat < 1 or args.threads < 0:
        parser.error("size, rounds and repeat must be positive; threads nonnegative")
    if args.queries < 8 or args.queries & (args.queries - 1):
        parser.error("queries must be a power of two, at least eight")
    checksums = {}
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
                    if checksums.setdefault(equivalent, checksum) != checksum:
                        raise RuntimeError(f"checksum mismatch for {language} {name}")
                    cases.append({"name": name, "ns_per_op": float(elapsed), "checksum": checksum})
                if not cases:
                    raise RuntimeError("benchmark returned no operations")
                print(json.dumps({"language": language, "kind": kind, "repeat": repeat, "entries": args.size, "queries": args.queries, "rounds": args.rounds, "threads": args.threads, "mutations": args.mutations, "distinct": args.distinct, "build": lines[0], "cases": cases}), flush=True)


if __name__ == "__main__":
    main()
