use bbtree::unsync::BBTreeSet;
use std::io::{self, BufRead, Write};

pub fn run() {
    let mut sets = [BBTreeSet::<u64>::new(), BBTreeSet::<u64>::new()];
    let mut out = io::BufWriter::new(io::stdout().lock());
    for line in io::stdin().lock().lines() {
        let line = line.unwrap();
        let args: Vec<&str> = line.split_whitespace().collect();
        let number = |i: usize| args[i].parse::<u64>().unwrap();
        match args[0] {
            "I" => writeln!(out, "{}", sets[number(1) as usize].insert(number(2))).unwrap(),
            "D" => writeln!(out, "{}", sets[number(1) as usize].remove(&number(2))).unwrap(),
            "G" => writeln!(out, "{}", sets[number(1) as usize].contains(&number(2))).unwrap(),
            "P" => {
                let s = &mut sets[number(1) as usize];
                let value = if number(2) == 0 { s.pop_first() } else { s.pop_last() };
                writeln!(out, "{}", super::result(value)).unwrap();
            }
            "T" => {
                let s = &mut sets[number(1) as usize];
                s.retain(|k| k % 3 != 0);
                writeln!(out, "{}", s.len()).unwrap();
            }
            "E" => {
                let s = &mut sets[number(1) as usize];
                for key in s.extract_if(number(2)..number(3), |k| k % 2 == 0).take(number(4) as usize) {
                    write!(out, "{key},").unwrap();
                }
                writeln!(out).unwrap();
            }
            "S" => {
                sets[1] = sets[0].split_off(&number(1));
                writeln!(out, "{},{}", sets[0].len(), sets[1].len()).unwrap();
            }
            "A" => {
                let [left, right] = &mut sets;
                left.append(right);
                writeln!(out, "{}", left.len()).unwrap();
            }
            "L" => writeln!(out, "{},{}", sets[0].len(), sets[1].len()).unwrap(),
            op => {
                let values: Box<dyn Iterator<Item=&u64>> = match op {
                    "U" => Box::new(sets[0].union(&sets[1])),
                    "N" => Box::new(sets[0].intersection(&sets[1])),
                    "F" => Box::new(sets[0].difference(&sets[1])),
                    "Y" => Box::new(sets[0].symmetric_difference(&sets[1])),
                    _ => panic!("unknown set operation"),
                };
                for key in values { write!(out, "{key},").unwrap(); }
                writeln!(out).unwrap();
            }
        }
    }
}
