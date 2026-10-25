use bbtree::{BBTreeMap, unsync};
use std::{hint::black_box, time::Instant};
fn mix(mut x: u64) -> u64 {
    x ^= x >> 30;
    x = x.wrapping_mul(0xbf58476d1ce4e5b9);
    x ^= x >> 27;
    x = x.wrapping_mul(0x94d049bb133111eb);
    x ^ (x >> 31)
}
fn report(name: &str, start: Instant, sum: u64, rounds: usize) {
    let elapsed = start.elapsed().as_nanos() as f64 / (rounds * 8192) as f64;
    println!("{name} {elapsed:.3} ns/op checksum={}", black_box(sum));
}
fn main() {
    let args: Vec<String> = std::env::args().collect();
    let size: u64 = args.get(1).map_or(262144, |v| v.parse().unwrap());
    let rounds: usize = args.get(2).map_or(128, |v| v.parse().unwrap());
    let mut tree = unsync::BBTreeMap::<u64, u64>::new();
    let concurrent = BBTreeMap::<u64, u64>::new();
    for i in 0..size {
        let k = mix(i);
        tree.insert(k, k);
        concurrent.insert(k, k).unwrap();
    }
    println!("upstream 8adae97 n={size} rounds={rounds}");
    let mut queries = [0_u64; 8192];
    for hit in [true, false] {
        let label = if hit { "hit" } else { "miss" };
        for (i, q) in queries.iter_mut().enumerate() {
            let mut x = mix(i as u64 + 17) % size;
            if !hit {
                x += size;
            }
            *q = mix(x);
        }
        let mut sum = 0_u64;
        let start = Instant::now();
        for _ in 0..rounds {
            for q in &queries {
                sum = sum.wrapping_add(tree.get(black_box(q)).copied().unwrap_or(0));
            }
        }
        report(&format!("tree_{label}"), start, sum, rounds);
        sum = 0;
        let start = Instant::now();
        for _ in 0..rounds {
            for q in &queries {
                sum = sum.wrapping_add(concurrent.get_copied(black_box(q)).unwrap_or(0));
            }
        }
        report(&format!("map_{label}"), start, sum, rounds);
        sum = 0;
        let start = Instant::now();
        for _ in 0..rounds {
            for chunk in queries.chunks_exact(8) {
                let qs = std::array::from_fn::<_, 8, _>(|i| &chunk[i]);
                for v in tree.get_many(black_box(qs)) {
                    sum = sum.wrapping_add(v.copied().unwrap_or(0));
                }
            }
        }
        report(&format!("tree_batch8_{label}"), start, sum, rounds);
    }
}
