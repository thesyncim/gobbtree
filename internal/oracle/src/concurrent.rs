use bbtree::BBTreeMap;
use std::io::{self, BufRead, Write};
use std::ops::ControlFlow;

use super::{bound, or_panic, result};

/// The concurrent map's read-modify-write operations and scans, one thread.
pub fn run() {
    let mut map = BBTreeMap::<u64, u64>::new();
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
            "R" => writeln!(out, "{}", map.read(&number(1), |k, v| format!("{k}:{v}")).unwrap_or("-".to_owned())).unwrap(),
            "A" => {
                let line = map.remove_entry(&number(1)).map_or("-".to_owned(), |(k, v)| format!("{k}:{v}"));
                writeln!(out, "{line}").unwrap();
            }
            "H" => writeln!(out, "{}", map.contains_key(&number(1))).unwrap(),
            "E" => writeln!(out, "{}", map.is_empty()).unwrap(),
            "C" => {
                map.clear();
                writeln!(out, "{}", map.is_empty()).unwrap();
            }
            "GM" => {
                let got = map.get_many_copied([&number(1), &number(2), &number(3), &number(4)]);
                writeln!(out, "{}", got.iter().map(|v| result(*v) + ",").collect::<String>()).unwrap();
            }
            "RM" => {
                let got = map.read_many([&number(1), &number(2), &number(3), &number(4)], |k, v| format!("{k}:{v}"));
                writeln!(out, "{}", got.into_iter().map(|v| v.unwrap_or("-".to_owned()) + ",").collect::<String>()).unwrap();
            }
            "IM" => {
                let got = map.insert_many([(number(1), number(2)), (number(3), number(4)), (number(5), number(6)), (number(7), number(8))]);
                writeln!(out, "{}", got.into_iter().map(|v| result(v.unwrap()) + ",").collect::<String>()).unwrap();
            }
            "UM" => {
                let got = map.update_or_insert_many(
                    [(number(1), number(2)), (number(3), number(4)), (number(5), number(6)), (number(7), number(8))],
                    |_, stored, value| {
                        let old = *stored;
                        *stored = old.wrapping_add(value);
                        old
                    },
                );
                writeln!(out, "{}", got.into_iter().map(|v| result(v.unwrap()) + ",").collect::<String>()).unwrap();
            }
            "RC" => {
                let range = (bound(args[1], number(2)), bound(args[3], number(4)));
                or_panic(&mut out, || map.range_cloned(range).map(|(k, v)| format!("{k}:{v},")).collect());
            }
            "XM" => {
                let d = number(1);
                for (k, v) in map.iter_mut() {
                    *v = v.wrapping_add(k ^ d);
                }
                writeln!(out, "-").unwrap();
            }
            "GU" => {
                let line = match map.get_mut(&number(1)) {
                    Some(v) => {
                        *v ^= 3;
                        v.to_string()
                    }
                    None => "-".to_owned(),
                };
                writeln!(out, "{line}").unwrap();
            }
            "J" => {
                let taken: String = std::mem::take(&mut map).into_iter().take(number(1) as usize).map(|(k, v)| format!("{k}:{v},")).collect();
                writeln!(out, "{taken}").unwrap();
            }
            _ => panic!("unknown concurrent operation"),
        }
    }
}
