use bbtree::BBTreeMap;
use std::io::{self, BufRead, Write};
use std::ops::ControlFlow;

use super::{bound, or_panic, result};

/// The concurrent map's read-modify-write operations and scans, one thread.
pub fn run() {
    let map = BBTreeMap::<u64, u64>::new();
    let mut out = io::BufWriter::new(io::stdout().lock());
    for line in io::stdin().lock().lines() {
        let line = line.unwrap();
        let args: Vec<&str> = line.split_whitespace().collect();
        let number = |i: usize| args[i].parse::<u64>().unwrap();
        match args[0] {
            "I" => writeln!(out, "{}", result(map.insert(number(1), number(2)).unwrap())).unwrap(),
            "D" => writeln!(out, "{}", result(map.remove(&number(1)))).unwrap(),
            "G" => writeln!(out, "{}", result(map.get_copied(&number(1)))).unwrap(),
            "U" => {
                let old = map
                    .update_or_insert(number(1), number(2), |_, stored, value| {
                        let old = *stored;
                        *stored = old.wrapping_add(value);
                        old
                    })
                    .unwrap();
                writeln!(out, "{}", result(old)).unwrap();
            }
            "X" => writeln!(out, "{}", result(map.update(&number(1), |_, v| {
                *v ^= 1;
                *v
            })))
            .unwrap(),
            "O" => writeln!(out, "{}", map.get_or_insert(number(1), number(2), |_, v| *v).unwrap()).unwrap(),
            "F" => {
                let line = match map.remove_if(&number(1), |k, v| (k ^ v) % 2 == 0) {
                    Some((k, v)) => format!("{k}:{v}"),
                    None => "-".to_owned(),
                };
                writeln!(out, "{line}").unwrap();
            }
            "B" | "W" => {
                let range = (bound(args[1], number(2)), bound(args[3], number(4)));
                let reverse = args[0] == "W";
                or_panic(&mut out, || {
                    let mut line = String::new();
                    let visit = |k: &u64, v: &u64| {
                        line.push_str(&format!("{k}:{v},"));
                        ControlFlow::<()>::Continue(())
                    };
                    if reverse {
                        map.scan_rev(range, visit);
                    } else {
                        map.scan(range, visit);
                    }
                    line
                });
            }
            "M" => {
                let range = (bound(args[1], number(2)), bound(args[3], number(4)));
                or_panic(&mut out, || match map.first_copied(range) {
                    Some((k, v)) => format!("{k}:{v}"),
                    None => "-".to_owned(),
                });
            }
            _ => panic!("unknown concurrent operation"),
        }
    }
}
