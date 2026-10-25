use bbtree::unsync::BBTreeMap;
use std::io::{self, BufRead, Write};

fn result(v: Option<u64>) -> String {
    v.map_or_else(|| "-".to_owned(), |v| v.to_string())
}

fn main() {
    let mut tree = BBTreeMap::<u64, u64>::new();
    let stdin = io::stdin();
    let mut out = io::BufWriter::new(io::stdout().lock());
    for line in stdin.lock().lines() {
        let line = line.unwrap();
        let mut parts = line.split_whitespace();
        let op = parts.next().unwrap();
        let args: Vec<u64> = parts.map(|p| p.parse().unwrap()).collect();
        match op {
            "I" => writeln!(out, "{}", result(tree.insert(args[0], args[1]))).unwrap(),
            "D" => writeln!(out, "{}", result(tree.remove(&args[0]))).unwrap(),
            "G" => writeln!(out, "{}", result(tree.get(&args[0]).copied())).unwrap(),
            "R" => {
                for (k, v) in tree.range(args[0]..args[1]) {
                    write!(out, "{k}:{v},").unwrap();
                }
                writeln!(out).unwrap();
            }
            "V" => {
                for (k, v) in tree.range(args[0]..args[1]).rev() {
                    write!(out, "{k}:{v},").unwrap();
                }
                writeln!(out).unwrap();
            }
            "L" => writeln!(out, "{}", tree.len()).unwrap(),
            "C" => { tree.clear(); writeln!(out, "-").unwrap(); }
            _ => panic!("unknown operation"),
        }
    }
}
