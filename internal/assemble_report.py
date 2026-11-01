"""Gather a matrix run's segments (internal/matrix.sh) into a report.

assemble_report.py <dir> <old report> <new report> <go source> <limitations json>

The old report gives the fields a run doesn't change: hardware, toolchains
and method. The limitations are this run's, a JSON list of sentences.
"""
import json
import sys
from pathlib import Path


def records(paths):
    out = []
    for path in sorted(paths):
        for line in path.read_text().splitlines():
            if line.startswith("{") and "language" in (row := json.loads(line)):
                out.append(row)
    return out


def main():
    run, old, new, go_source, limitations = sys.argv[1:6]
    segments = Path(run) / "segments"
    report = json.loads(Path(old).read_text())
    report["date"] = new.rsplit("arm64-", 1)[-1].removesuffix(".json")
    report["go_source"] = go_source
    report["limitations"] = json.loads(Path(limitations).read_text())
    for field, prefixes in (("cpu", ("cpu-", "text-")), ("rss", ("rss-",)), ("store", ("store-",))):
        report[field] = records(p for p in segments.glob("*.jsonl") if p.name.startswith(prefixes))
    Path(new).write_text(json.dumps(report, indent=1) + "\n")
    print(len(report["cpu"]), "cpu,", len(report["rss"]), "rss,", len(report["store"]), "store records")


if __name__ == "__main__":
    main()
