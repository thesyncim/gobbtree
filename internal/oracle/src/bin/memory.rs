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
use std::time::Instant;
use std::{
    hint::black_box,
    io::{self, Read},
};
fn mix(mut x: u64) -> u64 {
    x ^= x >> 30;
    x = x.wrapping_mul(0xbf58476d1ce4e5b9);
    x ^= x >> 27;
    x = x.wrapping_mul(0x94d049bb133111eb);
    x ^ (x >> 31)
}
// Stored key x in a key pattern, matching cmd/memory.
fn key_of(x: u64, pattern: &str) -> u64 {
    match pattern {
        "sequential" => x << 1,
        "clustered" => mix(x >> 10) & !((1 << 20) - 1) | (x & 1023) << 1,
        _ => mix(x),
    }
}
fn wait() {
    io::stdin().read_exact(&mut [0_u8]).unwrap();
}
fn report(kind: &str, phase: &str, size: u64, sum: u64, build_ns: u128) {
    println!("{{\"allocator\":\"{ALLOCATOR_NAME}\",\"pid\":{},\"build_ns\":{build_ns},\"kind\":\"{kind}\",\"phase\":\"{phase}\",\"entries\":{size},\"checksum\":{sum}}}",std::process::id());
}
fn measure<T>(keep: T, kind: &str, size: u64, build_ns: u128, read: impl FnOnce(&T) -> u64) {
    report(kind, "built", size, 0, build_ns);
    wait();
    let sum = read(black_box(&keep));
    report(kind, "read", size, black_box(sum), build_ns);
    wait();
    report(kind, "settled", size, black_box(sum), build_ns);
    wait();
    black_box(&keep);
}
fn main() {
    let args: Vec<String> = std::env::args().collect();
    let kind = args.get(1).map(String::as_str).unwrap_or("tree");
    let size = args.get(2).map_or(5_000_000, |v| v.parse::<u64>().unwrap());
    let pattern = args.get(3).map(String::as_str).unwrap_or("mixed");
    assert!(matches!(pattern, "mixed" | "sequential" | "clustered"));
    let key = |x: u64| key_of(x, pattern);
    let start = Instant::now();
    match kind {
        "tree" => {
            let mut tree = unsync::BBTreeMap::<u64, u64>::new();
            for i in 0..size {
                let k = key(i);
                tree.insert(k, k);
            }
            let build_ns = start.elapsed().as_nanos();
            measure(tree, kind, size, build_ns, |tree| {
                let mut sum = 0_u64;
                for i in 0..size {
                    sum = sum.wrapping_add(tree.get(&key(i)).copied().unwrap());
                }
                sum
            });
        }
        "map" => {
            let tree = BBTreeMap::<u64, u64>::new();
            for i in 0..size {
                let k = key(i);
                tree.insert(k, k).unwrap();
            }
            let build_ns = start.elapsed().as_nanos();
            measure(tree, kind, size, build_ns, |tree| {
                let mut sum = 0_u64;
                for i in 0..size {
                    sum = sum.wrapping_add(tree.get_copied(&key(i)).unwrap());
                }
                sum
            });
        }
        _ => panic!("unknown kind"),
    }
}
