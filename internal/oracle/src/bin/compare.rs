// Match the allocator used by the pinned upstream benchmark executable.
#[cfg(not(feature = "system-allocator"))]
#[global_allocator]
static ALLOCATOR: mimalloc::MiMalloc = mimalloc::MiMalloc;
const ALLOCATOR_NAME: &str = if cfg!(feature = "system-allocator") {
    "system"
} else {
    "mimalloc"
};

use bbtree::{unsync, BBTreeMap};
use std::{hint::black_box, time::Instant};
fn mix(mut x: u64) -> u64 {
    x ^= x >> 30;
    x = x.wrapping_mul(0xbf58476d1ce4e5b9);
    x ^= x >> 27;
    x = x.wrapping_mul(0x94d049bb133111eb);
    x ^ (x >> 31)
}
// The key at index x, matching keyOf in cmd/compare: indexes below size are
// stored, and size+x is the miss or fresh key paired with x.
fn key_of(mut x: u64, size: u64, pattern: &str) -> u64 {
    let mut miss = 0;
    if x >= size {
        x -= size;
        miss = 1;
    }
    match pattern {
        "sequential" => x << 1 | miss,
        "clustered" => mix(x >> 10) & !((1 << 20) - 1) | (x & 1023) << 1 | miss,
        _ => mix(x + miss * size),
    }
}
fn report(name: &str, start: Instant, sum: u64, rounds: usize, queries: usize) {
    let elapsed = start.elapsed().as_nanos() as f64 / (rounds * queries) as f64;
    println!("{name} {elapsed:.3} ns/op checksum={}", black_box(sum));
}
// Keep final-value validation outside the reported replacement time.
fn verify_replacements(queries: &[u64], rounds: usize, get: impl Fn(&u64) -> Option<u64>) {
    let mask = ((rounds - 1) & 1) as u64;
    for q in queries {
        assert_eq!(
            get(q),
            Some(*q ^ mask),
            "replacement final value for key {q}"
        );
    }
}
// The callback runs once per worker, outside the per-operation dispatch.
// Worker creation and final checksum reduction are untimed. Returns the
// elapsed nanoseconds and the wrapping sum of the workers' results.
fn parallel(threads: usize, work: impl Fn(usize) -> u64 + Sync) -> (f64, u64) {
    let ready = std::sync::Barrier::new(threads + 1);
    let barrier = std::sync::Barrier::new(threads + 1);
    let done = std::sync::Barrier::new(threads + 1);
    std::thread::scope(|scope| {
        let mut handles = Vec::new();
        for worker in 0..threads {
            let (ready, barrier, done, work) = (&ready, &barrier, &done, &work);
            handles.push(scope.spawn(move || {
                ready.wait();
                barrier.wait();
                let sum = work(worker);
                done.wait();
                sum
            }));
        }
        ready.wait();
        let start = Instant::now();
        barrier.wait();
        done.wait();
        let elapsed = start.elapsed().as_nanos() as f64;
        let mut sum = 0_u64;
        for handle in handles {
            sum = sum.wrapping_add(handle.join().unwrap());
        }
        (elapsed, sum)
    })
}
// The first step at or above a golden-ratio fraction of n that is coprime to
// n, matching coprimeStep in cmd/compare.
fn coprime_step(n: u64) -> u64 {
    let mut step = 0x9e3779b97f4a7c15_u64 % n;
    loop {
        let (mut a, mut b) = (step, n);
        while b != 0 {
            (a, b) = (b, a % b);
        }
        if a == 1 {
            return step;
        }
        step += 1;
    }
}
fn main() {
    let args: Vec<String> = std::env::args().collect();
    let size: u64 = args.get(1).map_or(262144, |v| v.parse().unwrap());
    let rounds: usize = args.get(2).map_or(128, |v| v.parse().unwrap());
    let query_count: usize = args.get(4).map_or(8192, |v| v.parse().unwrap());
    assert!(size > 0 && rounds > 0 && query_count >= 8 && query_count.is_power_of_two());
    let kind = args.get(5).map(String::as_str).unwrap_or("all");
    assert!(matches!(kind, "all" | "tree" | "map"));
    let pattern = args
        .iter()
        .find_map(|arg| arg.strip_prefix("keys="))
        .unwrap_or("mixed");
    assert!(matches!(pattern, "mixed" | "sequential" | "clustered"));
    let key = |x: u64| key_of(x, size, pattern);
    let scale_mix = args.iter().find_map(|arg| arg.strip_prefix("scale="));
    let ops: usize = args
        .iter()
        .find_map(|arg| arg.strip_prefix("ops="))
        .map_or(1 << 20, |v| v.parse().unwrap());
    // A core-scaling cell builds only the concurrent map.
    let kind = if scale_mix.is_some() { "map" } else { kind };
    let do_tree = kind == "all" || kind == "tree";
    let do_map = kind == "all" || kind == "map";
    let mut tree = unsync::BBTreeMap::<u64, u64>::new();
    let concurrent = BBTreeMap::<u64, u64>::new();
    for i in 0..size {
        let k = key(i);
        if do_tree {
            tree.insert(k, k);
        }
        if do_map {
            concurrent.insert(k, k).unwrap();
        }
    }
    println!("upstream 1c72bd2 allocator={ALLOCATOR_NAME} n={size} rounds={rounds} queries={query_count} kind={kind} keys={pattern}");
    let mut queries = vec![0_u64; query_count];
    if let Some(name) = scale_mix {
        let threads: usize = args.get(3).map_or(0, |v| v.parse().unwrap());
        assert!(
            threads > 0 && ops > 0,
            "scale needs positive threads and ops"
        );
        for (i, q) in queries.iter_mut().enumerate() {
            *q = key(mix(i as u64 + 17) % size);
        }
        scale(&concurrent, name, threads, ops, &queries, size, &key);
        return;
    }
    for hit in [true, false] {
        let label = if hit { "hit" } else { "miss" };
        for (i, q) in queries.iter_mut().enumerate() {
            let mut x = mix(i as u64 + 17) % size;
            if !hit {
                x += size;
            }
            *q = key(x);
        }
        if do_tree {
            let mut sum = 0_u64;
            let start = Instant::now();
            for _ in 0..rounds {
                for q in black_box(&queries) {
                    sum = sum.wrapping_add(tree.get(q).copied().unwrap_or(0));
                }
            }
            report(&format!("tree_{label}"), start, sum, rounds, query_count);
        }
        if do_map {
            let mut sum = 0_u64;
            let start = Instant::now();
            for _ in 0..rounds {
                for q in black_box(&queries) {
                    sum = sum.wrapping_add(concurrent.get_copied(q).unwrap_or(0));
                }
            }
            report(&format!("map_{label}"), start, sum, rounds, query_count);
        }
        if do_tree {
            let mut sum = 0_u64;
            let start = Instant::now();
            for _ in 0..rounds {
                for chunk in black_box(&queries).chunks_exact(8) {
                    let qs = std::array::from_fn::<_, 8, _>(|i| &chunk[i]);
                    for v in tree.get_many(qs) {
                        sum = sum.wrapping_add(v.copied().unwrap_or(0));
                    }
                }
            }
            report(
                &format!("tree_batch8_{label}"),
                start,
                sum,
                rounds,
                query_count,
            );
        }
        if do_map {
            let mut sum = 0_u64;
            let start = Instant::now();
            for _ in 0..rounds {
                for chunk in black_box(&queries).chunks_exact(8) {
                    let qs = std::array::from_fn::<_, 8, _>(|i| &chunk[i]);
                    for v in concurrent.get_many_copied(qs) {
                        sum = sum.wrapping_add(v.unwrap_or(0));
                    }
                }
            }
            report(
                &format!("map_batch8_{label}"),
                start,
                sum,
                rounds,
                query_count,
            );
        }
    }
    let threads: usize = args.get(3).map_or(0, |v| v.parse().unwrap());
    if threads > 0 && do_map {
        for (i, q) in queries.iter_mut().enumerate() {
            *q = key(mix(i as u64 + 17) % size);
        }
        for write_every in [0_usize, 10] {
            let ready = std::sync::Barrier::new(threads + 1);
            let barrier = std::sync::Barrier::new(threads + 1);
            let done = std::sync::Barrier::new(threads + 1);
            std::thread::scope(|scope| {
                let mut handles = Vec::new();
                for worker in 0..threads {
                    let ready = &ready;
                    let barrier = &barrier;
                    let done = &done;
                    let queries = &queries;
                    let concurrent = &concurrent;
                    handles.push(scope.spawn(move || {
                        ready.wait();
                        barrier.wait();
                        let mut sum = 0_u64;
                        for round in 0..rounds {
                            let queries = black_box(queries);
                            for i in 0..queries.len() {
                                let q = queries[(i + worker * 113) & (query_count - 1)];
                                if write_every != 0 && i % 10 == 0 {
                                    concurrent.insert(q, q ^ ((round & 1) as u64)).unwrap();
                                } else {
                                    sum = sum
                                        .wrapping_add(concurrent.get_copied(&q).unwrap_or(0) & !1);
                                }
                            }
                        }
                        done.wait();
                        sum
                    }));
                }
                ready.wait();
                let start = Instant::now();
                barrier.wait();
                done.wait();
                let elapsed =
                    start.elapsed().as_nanos() as f64 / (threads * rounds * query_count) as f64;
                let mut sum = 0_u64;
                for handle in handles {
                    sum = sum.wrapping_add(handle.join().unwrap());
                }
                let label = if write_every == 0 { "hit" } else { "write10" };
                println!(
                    "map_parallel{threads}_{label} {elapsed:.3} ns/op checksum={}",
                    black_box(sum)
                );
            });
        }
    }
    if args.get(6).is_some_and(|arg| arg == "mutations") {
        for (i, q) in queries.iter_mut().enumerate() {
            *q = key(mix(i as u64 + 17) % size);
        }
        for churn in [false, true] {
            let label = if churn { "churn_pair" } else { "replace" };
            if do_tree {
                for q in &queries {
                    tree.insert(*q, *q);
                }
                let mut sum = 0_u64;
                let start = Instant::now();
                for r in 0..rounds {
                    for q in black_box(&queries) {
                        if churn {
                            sum = sum.wrapping_add(tree.remove(q).unwrap_or(0) & !1);
                            tree.insert(*q, *q);
                        } else {
                            sum =
                                sum.wrapping_add(tree.insert(*q, *q ^ (r as u64 & 1)).unwrap_or(0));
                        }
                    }
                }
                report(&format!("tree_{label}"), start, sum, rounds, query_count);
                if !churn {
                    verify_replacements(&queries, rounds, |q| tree.get(q).copied());
                }
            }
            if do_map {
                for q in &queries {
                    concurrent.insert(*q, *q).unwrap();
                }
                let mut sum = 0_u64;
                let start = Instant::now();
                for r in 0..rounds {
                    for q in black_box(&queries) {
                        if churn {
                            sum = sum.wrapping_add(concurrent.remove(q).unwrap_or(0) & !1);
                            concurrent.insert(*q, *q).unwrap();
                        } else {
                            sum = sum.wrapping_add(
                                concurrent
                                    .insert(*q, *q ^ (r as u64 & 1))
                                    .unwrap()
                                    .unwrap_or(0),
                            );
                        }
                    }
                }
                report(&format!("map_{label}"), start, sum, rounds, query_count);
                if !churn {
                    verify_replacements(&queries, rounds, |q| concurrent.get_copied(q));
                }
            }
        }
    }
    if args.iter().any(|arg| arg == "distinct") {
        let count = query_count.min(size as usize);
        let churn_rounds = rounds * (query_count / count);
        queries.truncate(count);
        let mut other = vec![0_u64; count];
        let step = coprime_step(size);
        let mut x = 17 % size;
        for (i, q) in queries.iter_mut().enumerate() {
            *q = key(x);
            other[i] = key(x + size);
            x = (x + step) % size;
        }
        if do_tree {
            for q in &queries {
                tree.insert(*q, *q);
            }
            let mut sum = 0_u64;
            let start = Instant::now();
            for r in 0..churn_rounds {
                let (remove, add) = if r & 1 == 0 {
                    (&queries, &other)
                } else {
                    (&other, &queries)
                };
                for (q, new_key) in black_box(remove).iter().zip(black_box(add)) {
                    let value = tree
                        .remove(q)
                        .expect("distinct churn must delete an existing key");
                    assert_eq!(value, *q);
                    sum = sum.wrapping_add(value);
                    assert!(tree.insert(*new_key, *new_key).is_none());
                }
            }
            report("tree_churn_new_pair", start, sum, churn_rounds, count);
            assert_eq!(tree.len(), size as usize);
        }
        if do_map {
            for q in &queries {
                concurrent.insert(*q, *q).unwrap();
            }
            let mut sum = 0_u64;
            let start = Instant::now();
            for r in 0..churn_rounds {
                let (remove, add) = if r & 1 == 0 {
                    (&queries, &other)
                } else {
                    (&other, &queries)
                };
                for (q, new_key) in black_box(remove).iter().zip(black_box(add)) {
                    let value = concurrent
                        .remove(q)
                        .expect("distinct churn must delete an existing key");
                    assert_eq!(value, *q);
                    sum = sum.wrapping_add(value);
                    assert!(concurrent.insert(*new_key, *new_key).unwrap().is_none());
                }
            }
            report("map_churn_new_pair", start, sum, churn_rounds, count);
            assert_eq!(concurrent.range_cloned::<u64, _>(..).count(), size as usize);
        }
    }
}

// The period of owner writes among a scaling worker's operations, matching
// scaleMixes in cmd/compare: 0 reads only, 1 writes only.
fn scale_write_every(name: &str) -> usize {
    match name {
        "read" => 0,
        "r90" | "scan" => 10,
        "r50" => 2,
        "replace" | "churn" => 1,
        _ => panic!("unknown scale mix {name}"),
    }
}
// How many entries each scan operation visits.
const SCAN_LENGTH: usize = 100;
// The keys worker owns, in its visit order, and the fresh key paired with
// each, matching ownedKeys in cmd/compare.
fn owned_keys(
    worker: usize,
    workers: usize,
    size: u64,
    key: &impl Fn(u64) -> u64,
) -> (Vec<u64>, Vec<u64>) {
    let owned = (size - worker as u64).div_ceil(workers as u64);
    let (step, mut slot) = (coprime_step(owned), mix(worker as u64 + 1) % owned);
    let mut stored = Vec::with_capacity(owned as usize);
    let mut fresh = Vec::with_capacity(owned as usize);
    for _ in 0..owned {
        let x = worker as u64 + slot * workers as u64;
        stored.push(key(x));
        fresh.push(key(x + size));
        slot = (slot + step) % owned;
    }
    (stored, fresh)
}
// A final entry's contribution to the state checksum.
fn scale_state(key: u64, value: u64) -> u64 {
    mix(key) ^ value
}
// The final state checksum derived from one worker's write schedule,
// independently of the map, matching expectedScaleState in cmd/compare.
fn expected_scale_state(name: &str, stored: &[u64], fresh: &[u64], writes: usize) -> u64 {
    let owned = stored.len();
    let (passes, partial) = (writes / owned, writes % owned);
    let mut state = 0_u64;
    if name != "churn" {
        for (t, &k) in stored.iter().enumerate() {
            let touches = passes + usize::from(t < partial);
            state = state.wrapping_add(scale_state(k, k ^ (touches & 1) as u64));
        }
        return state;
    }
    // After whole passes one key set is present; the partial pass deleted a
    // prefix of it and inserted the other set from half an orbit ahead.
    let (present, absent) = if passes & 1 == 0 {
        (stored, fresh)
    } else {
        (fresh, stored)
    };
    for &k in present {
        state = state.wrapping_add(scale_state(k, k));
    }
    for t in 0..partial {
        let k = absent[(t + owned / 2) % owned];
        state = state
            .wrapping_add(scale_state(k, k))
            .wrapping_sub(scale_state(present[t], present[t]));
    }
    state
}
// One core-scaling cell, matching scale in cmd/compare: workers run ops
// operations each, writing only keys they own and reading the shared queries
// buffer from per-worker offsets. Building keys, starting workers, reducing
// checksums and verifying the final state are untimed.
//
// The scan cell uses range_cloned(k..).take(100): Go's Map.Scan has
// range_cloned's contract (entries are copied and no latch is held while the
// caller's loop body runs), whereas upstream's scan runs its closure under
// each leaf's read latch.
fn scale(
    map: &BBTreeMap<u64, u64>,
    name: &str,
    workers: usize,
    ops: usize,
    queries: &[u64],
    size: u64,
    key: &(impl Fn(u64) -> u64 + Sync),
) {
    let write_every = scale_write_every(name);
    assert!(
        size >= workers as u64,
        "scale needs at least one key per worker"
    );
    let owned: Vec<(Vec<u64>, Vec<u64>)> = std::thread::scope(|scope| {
        let handles: Vec<_> = (0..workers)
            .map(|worker| scope.spawn(move || owned_keys(worker, workers, size, key)))
            .collect();
        handles.into_iter().map(|h| h.join().unwrap()).collect()
    });
    let (mask, scan, churn) = (queries.len() - 1, name == "scan", name == "churn");
    let (elapsed, sum) = parallel(workers, |worker| {
        let mut sum = 0_u64;
        let (stored, fresh) = (&owned[worker].0, &owned[worker].1);
        let offset = worker * queries.len() / workers;
        let (mut t, mut flip) = (0, 1_u64);
        if churn {
            // Each pass deletes every key of one set in visit order and inserts
            // the other set's, then the sets swap roles.
            let (mut remove, mut add) = (stored, fresh);
            let mut u = stored.len() / 2;
            for _ in 0..ops {
                let q = remove[t];
                let v = map.remove(&q).expect("churn must delete an existing key");
                assert_eq!(v, q);
                sum = sum.wrapping_add(v);
                assert!(
                    map.insert(add[u], add[u]).unwrap().is_none(),
                    "churn must insert a new key"
                );
                u += 1;
                if u == add.len() {
                    u = 0;
                }
                t += 1;
                if t == remove.len() {
                    (t, remove, add) = (0, add, remove);
                }
            }
            return sum;
        }
        let queries = black_box(queries);
        for j in 0..ops {
            if write_every != 0 && j % write_every == 0 {
                let k = stored[t];
                sum = sum.wrapping_add(map.insert(k, k ^ flip).unwrap().unwrap_or(0));
                t += 1;
                if t == stored.len() {
                    (t, flip) = (0, flip ^ 1);
                }
                continue;
            }
            let q = queries[(j + offset) & mask];
            if scan {
                for (_, v) in map.range_cloned::<u64, _>(q..).take(SCAN_LENGTH) {
                    sum = sum.wrapping_add(v & !1);
                }
                continue;
            }
            sum = sum.wrapping_add(map.get_copied(&q).unwrap_or(0) & !1);
        }
        sum
    });
    let writes = if write_every == 0 {
        0
    } else {
        ops.div_ceil(write_every)
    };
    let mut want = 0_u64;
    for (stored, fresh) in &owned {
        want = want.wrapping_add(expected_scale_state(name, stored, fresh, writes));
    }
    let (mut state, mut count) = (0_u64, 0_u64);
    for (k, v) in map.range_cloned::<u64, _>(..) {
        state = state.wrapping_add(scale_state(k, v));
        count += 1;
    }
    assert_eq!(count, size, "scale {name} final count");
    assert_eq!(state, want, "scale {name} final state");
    let elapsed = elapsed / (workers * ops) as f64;
    println!(
        "scale_{name}_w{workers} {elapsed:.3} ns/op checksum={} state={state} count={count}",
        black_box(sum)
    );
}
