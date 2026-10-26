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
fn report(name: &str, start: Instant, sum: u64, rounds: usize, queries: usize) {
    let elapsed = start.elapsed().as_nanos() as f64 / (rounds * queries) as f64;
    println!("{name} {elapsed:.3} ns/op checksum={}", black_box(sum));
}
fn main() {
    let args: Vec<String> = std::env::args().collect();
    let size: u64 = args.get(1).map_or(262144, |v| v.parse().unwrap());
    let rounds: usize = args.get(2).map_or(128, |v| v.parse().unwrap());
    let query_count: usize = args.get(4).map_or(8192, |v| v.parse().unwrap());
    assert!(size > 0 && rounds > 0 && query_count >= 8 && query_count.is_power_of_two());
    let kind = args.get(5).map(String::as_str).unwrap_or("all");
    assert!(matches!(kind, "all" | "tree" | "map"));
    let do_tree = kind == "all" || kind == "tree";
    let do_map = kind == "all" || kind == "map";
    let mut tree = unsync::BBTreeMap::<u64, u64>::new();
    let concurrent = BBTreeMap::<u64, u64>::new();
    for i in 0..size {
        let k = mix(i);
        if do_tree {
            tree.insert(k, k);
        }
        if do_map {
            concurrent.insert(k, k).unwrap();
        }
    }
    println!("upstream 8adae97 allocator={ALLOCATOR_NAME} n={size} rounds={rounds} queries={query_count} kind={kind}");
    let mut queries = vec![0_u64; query_count];
    for hit in [true, false] {
        let label = if hit { "hit" } else { "miss" };
        for (i, q) in queries.iter_mut().enumerate() {
            let mut x = mix(i as u64 + 17) % size;
            if !hit {
                x += size;
            }
            *q = mix(x);
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
    }
    let threads: usize = args.get(3).map_or(0, |v| v.parse().unwrap());
    if threads > 0 && do_map {
        for (i, q) in queries.iter_mut().enumerate() {
            *q = mix(mix(i as u64 + 17) % size);
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
}
