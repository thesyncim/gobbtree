use bbtree::unsync::BBTreeMap;
use bbtree::unsync::bbtree_map::Entry;
use std::io::{self, BufRead, Write};

use super::result;

fn pair(p: Option<(&u64, &u64)>) -> String {
    p.map_or_else(|| "-".to_owned(), |(k, v)| format!("{k}:{v}"))
}
fn owned(p: Option<(u64, u64)>) -> String {
    p.map_or_else(|| "-".to_owned(), |(k, v)| format!("{k}:{v}"))
}
fn list<'a>(entries: impl Iterator<Item = (&'a u64, &'a u64)>) -> String {
    entries.map(|(k, v)| format!("{k}:{v},")).collect()
}

/// Every single-owner map operation Go mirrors, on u64 keys and values.
pub fn run() {
    let mut tree = BBTreeMap::<u64, u64>::new();
    let mut out = io::BufWriter::new(io::stdout().lock());
    for line in io::stdin().lock().lines() {
        let line = line.unwrap();
        let args: Vec<&str> = line.split_whitespace().collect();
        let n = |i: usize| args[i].parse::<u64>().unwrap();
        let text = match args[0] {
            "I" => result(tree.insert(n(1), n(2))),
            "D" => result(tree.remove(&n(1))),
            "G" => result(tree.get(&n(1)).copied()),
            "K" => pair(tree.get_key_value(&n(1))),
            "H" => tree.contains_key(&n(1)).to_string(),
            "A" => owned(tree.remove_entry(&n(1))),
            "F" => pair(tree.first_key_value()),
            "Z" => pair(tree.last_key_value()),
            "P" => owned(if n(1) == 0 { tree.pop_first() } else { tree.pop_last() }),
            "N" => tree.entry(n(1)).or_insert(n(2)).to_string(),
            "O" => {
                let d = n(2);
                tree.entry(n(1)).and_modify(|v| *v = v.wrapping_add(d)).or_insert(d).to_string()
            }
            "W" => tree.entry(n(1)).or_default().to_string(),
            "Wk" => tree.entry(n(1)).or_insert_with_key(|k| k ^ 0xff).to_string(),
            "Q" => match tree.entry(n(1)) {
                Entry::Occupied(mut o) => format!("o:{}", o.insert(n(2))),
                Entry::Vacant(v) => format!("v:{}", v.insert(n(2))),
            },
            "Qe" => {
                let e = tree.entry(n(1)).insert_entry(n(2));
                format!("{}:{}", e.key(), e.get())
            }
            "Y" => match tree.entry(n(1)) {
                Entry::Occupied(o) => owned(Some(o.remove_entry())),
                Entry::Vacant(_) => "-".to_owned(),
            },
            "B" => {
                let entry = if n(1) == 0 { tree.first_entry() } else { tree.last_entry() };
                owned(entry.map(|o| o.remove_entry()))
            }
            "E" => {
                let m = n(3);
                let taken: Vec<_> = tree.extract_if(n(1)..n(2), |k, v| (*k ^ *v) % m == 0).collect();
                taken.iter().map(|(k, v)| format!("{k}:{v},")).collect()
            }
            "T" => {
                let m = n(1);
                tree.retain(|k, v| (*k ^ *v) % m != 0);
                tree.len().to_string()
            }
            "S" => {
                let mut high = tree.split_off(&n(1));
                let line = format!("{},{},{}", tree.len(), high.len(), pair(high.first_key_value()));
                tree.append(&mut high);
                format!("{line},{},{}", tree.len(), high.len())
            }
            "X" => {
                let d = n(1);
                for v in tree.values_mut() {
                    *v = v.wrapping_add(d);
                }
                tree.len().to_string()
            }
            "Xi" => {
                let d = n(1);
                for (k, v) in tree.iter_mut() {
                    *v = v.wrapping_add(k ^ d);
                }
                tree.len().to_string()
            }
            "R" => {
                let d = n(3);
                for (_, v) in tree.range_mut(n(1)..n(2)) {
                    *v = v.wrapping_add(d);
                }
                tree.len().to_string()
            }
            "M" => tree
                .get_many([&n(1), &n(2), &n(3), &n(4)])
                .iter()
                .map(|v| result(v.copied()) + ",")
                .collect(),
            "IM" => tree
                .insert_many([(n(1), n(2)), (n(3), n(4)), (n(5), n(6)), (n(7), n(8))])
                .iter()
                .map(|v| result(*v) + ",")
                .collect(),
            "C" => {
                let mut copy = tree.clone();
                let same = copy == tree;
                copy.insert(n(1), n(2));
                format!("{same},{},{:?}", copy == tree, tree.cmp(&copy) as i8)
            }
            "J" => tree.clone().into_iter().take(n(1) as usize).map(|(k, v)| format!("{k}:{v},")).collect(),
            "Jk" => tree.clone().into_keys().take(n(1) as usize).map(|k| format!("{k},")).collect(),
            "Jv" => tree.clone().into_values().take(n(1) as usize).map(|v| format!("{v},")).collect(),
            "V" => {
                let (keys, values) = (tree.keys().fold(0, |a, k| a ^ k), tree.values().fold(0, |a, v| a ^ v));
                format!("{keys},{values},{}", tree.iter().count())
            }
            "L" => list(tree.range(n(1)..n(2))),
            "Len" => tree.len().to_string(),
            "Clr" => {
                tree.clear();
                tree.is_empty().to_string()
            }
            _ => panic!("unknown full operation"),
        };
        writeln!(out, "{text}").unwrap();
    }
}
