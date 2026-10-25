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
fn wait() {
    io::stdin().read_exact(&mut [0_u8]).unwrap();
}
fn report(kind: &str, phase: &str, size: u64, sum: u64, build_ns: u128) {
    println!("{{\"pid\":{},\"build_ns\":{build_ns},\"kind\":\"{kind}\",\"phase\":\"{phase}\",\"entries\":{size},\"checksum\":{sum}}}",std::process::id());
}
fn measure<T>(keep: T, kind: &str, size: u64, sum: u64, build_ns: u128) {
    report(kind, "built", size, black_box(sum), build_ns);
    wait();
    report(kind, "settled", size, black_box(sum), build_ns);
    wait();
    black_box(&keep);
}
fn main() {
    let args: Vec<String> = std::env::args().collect();
    let kind = args.get(1).map(String::as_str).unwrap_or("tree");
    let size = args.get(2).map_or(5_000_000, |v| v.parse::<u64>().unwrap());
    let start = Instant::now();
    match kind {
        "tree" => {
            let mut tree = unsync::BBTreeMap::<u64, u64>::new();
            for i in 0..size {
                let k = mix(i);
                tree.insert(k, k);
            }
            let build_ns = start.elapsed().as_nanos();
            let mut sum = 0_u64;
            for i in 0..size.min(1024) {
                sum = sum.wrapping_add(tree.get(&mix(i)).copied().unwrap());
            }
            measure(tree, kind, size, sum, build_ns);
        }
        "map" => {
            let tree = BBTreeMap::<u64, u64>::new();
            for i in 0..size {
                let k = mix(i);
                tree.insert(k, k).unwrap();
            }
            let build_ns = start.elapsed().as_nanos();
            let mut sum = 0_u64;
            for i in 0..size.min(1024) {
                sum = sum.wrapping_add(tree.get_copied(&mix(i)).unwrap());
            }
            measure(tree, kind, size, sum, build_ns);
        }
        _ => panic!("unknown kind"),
    }
}
