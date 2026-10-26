use bbtree::unsync::BBTreeMap;
use std::io::{self, BufRead, Write};
mod sets;

fn result(v: Option<u64>) -> String {
    v.map_or_else(|| "-".to_owned(), |v| v.to_string())
}
fn decode_hex(s: &str) -> Vec<u8> {
    if s == "_" {
        return Vec::new();
    }
    s.as_bytes()
        .chunks_exact(2)
        .map(|p| u8::from_str_radix(std::str::from_utf8(p).unwrap(), 16).unwrap())
        .collect()
}
fn encode_hex(k: &[u8]) -> String {
    if k.is_empty() {
        return "_".to_owned();
    }
    k.iter().map(|b| format!("{b:02x}")).collect()
}
fn run<K: Ord + 'static>(decode: impl Fn(&str) -> K, encode: impl Fn(&K) -> String) {
    let mut tree = BBTreeMap::<K, u64>::new();
    let stdin = io::stdin();
    let mut out = io::BufWriter::new(io::stdout().lock());
    for line in stdin.lock().lines() {
        let line = line.unwrap();
        let args: Vec<&str> = line.split_whitespace().collect();
        match args[0] {
            "I" => writeln!(
                out,
                "{}",
                result(tree.insert(decode(args[1]), args[2].parse().unwrap()))
            )
            .unwrap(),
            "D" => writeln!(out, "{}", result(tree.remove(&decode(args[1])))).unwrap(),
            "G" => writeln!(out, "{}", result(tree.get(&decode(args[1])).copied())).unwrap(),
            "R" => {
                for (k, v) in tree.range(decode(args[1])..decode(args[2])) {
                    write!(out, "{}:{v},", encode(k)).unwrap();
                }
                writeln!(out).unwrap();
            }
            "V" => {
                for (k, v) in tree.range(decode(args[1])..decode(args[2])).rev() {
                    write!(out, "{}:{v},", encode(k)).unwrap();
                }
                writeln!(out).unwrap();
            }
            "L" => writeln!(out, "{}", tree.len()).unwrap(),
            "C" => {
                tree.clear();
                writeln!(out, "-").unwrap();
            }
            _ => panic!("unknown operation"),
        }
    }
}
fn main() {
    if std::env::args().nth(1).as_deref() == Some("sets") {
        sets::run();
    } else if std::env::args().nth(1).as_deref() == Some("strings") {
        run(decode_hex, |k: &Vec<u8>| encode_hex(k));
    } else {
        run(|s| s.parse::<u64>().unwrap(), |k| k.to_string());
    }
}
