"""Compare whole-process memory in isolated, identically populated children."""
import argparse
import json
import re
import subprocess
import sys
import tempfile
import time
from pathlib import Path


def measure(command):
    darwin = sys.platform == "darwin"
    if not darwin and sys.platform != "linux":
        raise RuntimeError("RSS comparison supports macOS and Linux")
    with tempfile.TemporaryFile(mode="w+") as errors:
        process = subprocess.Popen(
            ["/usr/bin/time", "-l" if darwin else "-v", *command],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=errors, text=True,
        )
        try:
            stages = []
            for phase in range(3):
                line = process.stdout.readline()
                if not line:
                    errors.seek(0)
                    raise RuntimeError(errors.read())
                stage = json.loads(line)
                if phase:
                    time.sleep(1)
                rss = subprocess.check_output(
                    ["ps", "-o", "rss=", "-p", str(stage["pid"])], text=True,
                )
                stage["rss_bytes"] = int(rss.strip()) * 1024
                stages.append(stage)
                process.stdin.write("\n")
                process.stdin.flush()
            if process.wait() != 0:
                errors.seek(0)
                raise RuntimeError(errors.read())
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()
        errors.seek(0)
        timing = errors.read()
    pattern = r"(\d+)\s+maximum resident set size" if darwin else r"Maximum resident set size \(kbytes\):\s*(\d+)"
    match = re.search(pattern, timing)
    if not match:
        raise RuntimeError(timing)
    return {"peak_rss_bytes": int(match[1]) * (1 if darwin else 1024), "stages": stages}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--go", required=True, type=Path)
    parser.add_argument("--rust", required=True, type=Path)
    parser.add_argument("--baseline", type=Path)
    parser.add_argument("--size", type=int, default=5_000_000)
    parser.add_argument("--repeat", type=int, default=3)
    parser.add_argument("--kinds", nargs="+", choices=["tree", "map", "builtin", "syncmap"], default=["tree", "map", "builtin", "syncmap"])
    args = parser.parse_args()
    if args.size < 0 or args.repeat < 1:
        parser.error("size must be nonnegative and repeat positive")
    checksums = {}
    for repeat in range(args.repeat):
        for kind in args.kinds:
            commands = [("go", [str(args.go.resolve()), "-kind", kind, "-size", str(args.size)])]
            if args.baseline and kind in ("tree", "map"):
                commands.insert(0, ("baseline", [str(args.baseline.resolve()), "-kind", kind, "-size", str(args.size)]))
            if kind in ("tree", "map"):
                commands.append(("rust", [str(args.rust.resolve()), kind, str(args.size)]))
            rotation = repeat % len(commands)
            commands = commands[rotation:] + commands[:rotation]
            for language, command in commands:
                result = measure(command)
                for stage in result["stages"]:
                    phase = "built" if stage["phase"] == "built" else "read"
                    if checksums.setdefault(phase, stage["checksum"]) != stage["checksum"] or stage["entries"] != args.size:
                        raise RuntimeError("mismatched dataset/checksum")
                print(json.dumps({"language": language, "kind": kind, "repeat": repeat, **result}), flush=True)


if __name__ == "__main__":
    main()
