#!/bin/zsh
# The README's benchmark matrix, a segment at a time, one at a time.
#
#   internal/matrix.sh <dir> [pattern]
#
# <dir> holds the drivers: compare-go, memory-go and storebench-go (Go),
# compare and memory (the Rust oracle's), and storebench-rust (upstream's
# bbtree-store examples/bench). Each segment's records go to
# <dir>/segments/<name>.jsonl, and its start, load average and spread to
# <dir>/log.txt. A timing segment, CPU or store, whose backend's runs spread
# past 1.3x (internal/spread.py) runs again in full, both languages, at most
# four times; memory segments' builds are single timings, kept as measured.
# A pattern runs only the segments whose names match it.
set -u
M=${1:a}; PATTERN=${2:-}
cd ${0:a:h}/..
mkdir -p $M/segments
CPU="python3 internal/cpu_compare.py --go $M/compare-go --rust $M/compare --threads 4 --repeat 3"
RSS="python3 internal/rss_compare.py --go $M/memory-go --rust $M/memory --repeat 3 --kinds tree map"
STORE="python3 internal/store_compare.py --go $M/storebench-go --rust $M/storebench-rust --repeat 3"
seg() { # seg <retake> <name> <cmd...>
  local retake=$1 name=$2; shift 2
  [[ -n $PATTERN && $name != *$PATTERN* ]] && return
  local out=$M/segments/${name// /-}.jsonl
  for try in 1 2 3 4; do
    echo "$(date +%H:%M:%S) start $name try $try load $(sysctl -n vm.loadavg)" >> $M/log.txt
    "$@" > $out 2>> $M/err.txt || { echo "$(date +%H:%M:%S) FAILED $name" >> $M/log.txt; return 1; }
    if python3 internal/spread.py $out 1.3 >> $M/log.txt || [[ $retake = once ]]; then return 0; fi
  done
  echo "$(date +%H:%M:%S) kept $name past the limit, after four tries" >> $M/log.txt
}
for size in 4096 262144; do
  for keys in mixed sequential clustered; do
    std=(); [[ $keys = mixed ]] && std=(--standard)
    seg retake "cpu $size $keys" ${=CPU} --size $size --queries 8192 --rounds 512 --mutations --distinct --keys $keys $std
  done
done
seg retake "text 262144" ${=CPU} --size 262144 --queries 8192 --rounds 512 --text --mutations
for keys in mixed sequential clustered; do
  std=(); [[ $keys = mixed ]] && std=(--standard)
  seg retake "cpu 5000000 $keys" ${=CPU} --size 5000000 --queries 1048576 --rounds 4 --mutations --distinct --keys $keys $std
done
seg retake "text 5000000" ${=CPU} --size 5000000 --queries 1048576 --rounds 4 --text --mutations
for keys in mixed sequential clustered; do
  seg retake "cpu 50000000 $keys" ${=CPU} --size 50000000 --queries 1048576 --rounds 4 --mutations --distinct --keys $keys
done
for size in 4096 262144 5000000 50000000; do
  for keys in mixed sequential clustered; do
    seg once "rss $size $keys" ${=RSS} --size $size --keys $keys
  done
done
for size in 32768 1000000 10000000; do
  seg retake "store $size" ${=STORE} --size $size
done
echo "$(date +%H:%M:%S) all done" >> $M/log.txt
