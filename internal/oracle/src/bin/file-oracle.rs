use bbtree_mmap::BBTreeMapFile;
use std::io::{self, BufRead};

#[derive(Clone, Copy, PartialEq, Eq)]
#[repr(transparent)]
struct ThreeWords([u64; 3]);
// Exactly three plain integers, with no padding, pointers or invalid bit patterns.
unsafe impl bbtree::file::PlainBytes for ThreeWords {}
bbtree_mmap::plain_disk_type!(ThreeWords);

fn mix(mut x: u64) -> u64 {
    x ^= x >> 30;
    x = x.wrapping_mul(0xbf58476d1ce4e5b9);
    x ^= x >> 27;
    x = x.wrapping_mul(0x94d049bb133111eb);
    x ^ (x >> 31)
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let args: Vec<String> = std::env::args().collect();
    let command = args.get(1).map(String::as_str).unwrap_or("");
    let path = args.get(2).ok_or("missing file path")?;
    if command == "write-tagged" {
        let size: u64 = args.get(3).ok_or("missing size")?.parse()?;
        let entries = (0..size).map(|i| (format!("shared/{i:016x}"), ThreeWords([i, i ^ u64::MAX, i * 7])));
        BBTreeMapFile::<String, ThreeWords>::create(path, entries)?;
        return Ok(());
    }
    if command == "dump-tagged" {
        let file = BBTreeMapFile::<String, ThreeWords>::read(path)?;
        println!("len {}", file.len());
        for (key, value) in file.iter() {
            println!("{key} {} {} {}", value.0[0], value.0[1], value.0[2]);
            if file.get(key) != Some(value) { return Err("tagged lookup mismatch".into()); }
        }
        return Ok(());
    }
    if command == "write-text" {
        let size: usize = args.get(3).ok_or("missing size")?.parse()?;
        let entries = (0..size).map(|i| {
            let key = if i == 0 { String::new() } else { format!("shared/prefix/{i:016x}/é\0") };
            let mut value = key.as_bytes().to_vec();
            value.extend_from_slice(&[0, 255, (i % 256) as u8]);
            (key, value)
        });
        BBTreeMapFile::<String, Vec<u8>>::create(path, entries)?;
        return Ok(());
    }
    if command == "dump-text" {
        let file = BBTreeMapFile::<String, Vec<u8>>::read(path)?;
        println!("len {}", file.len());
        for (key, value) in file.iter() {
            for b in key.bytes() { print!("{b:02x}"); }
            print!(" ");
            for b in value { print!("{b:02x}"); }
            println!();
            if file.get(key) != Some(value) { return Err("text lookup mismatch".into()); }
        }
        return Ok(());
    }
    if command == "write-small" {
        let size: i32 = args.get(3).ok_or("missing size")?.parse()?;
        let entries = (0..size).map(|i| (i - size / 2, [(i % 256) as u8; 13]));
        BBTreeMapFile::<i32, [u8; 13]>::create(path, entries)?;
        return Ok(());
    }
    if command == "dump-small" {
        let file = BBTreeMapFile::<i32, [u8; 13]>::read(path)?;
        println!("len {}", file.len());
        for (key, value) in file.iter() {
            print!("{key} ");
            for b in value { print!("{b:02x}"); }
            println!();
            if file.get(key) != Some(value) { return Err("small lookup mismatch".into()); }
        }
        return Ok(());
    }
    if command == "write" {
        let size: u64 = args.get(3).ok_or("missing size")?.parse()?;
        let mode = args.get(4).map(String::as_str).unwrap_or("mixed");
        let mut keys: Vec<u64> = (0..size)
            .map(|i| match mode {
                "dense" => i,
                "prefix" => (1_u64 << 63) | i * 7,
                "mixed" => mix(i),
                _ => panic!("unknown key mode"),
            })
            .collect();
        if mode == "mixed" && size > 1 {
            keys[(size - 1) as usize] = u64::MAX;
        }
        keys.sort_unstable();
        let entries = keys.into_iter().map(|k| (k, mix(k ^ 0xa5a5a5a5a5a5a5a5)));
        BBTreeMapFile::<u64, u64>::create(path, entries)?;
        return Ok(());
    }
    let file = BBTreeMapFile::<u64, u64>::read(path)?;
    if command == "dump" {
        println!("len {}", file.len());
        for (k, v) in file.iter() {
            println!("{k:016x} {v:016x}");
            if file.get(k) != Some(v) { return Err("lookup mismatch".into()); }
        }
        return Ok(());
    }
    if command != "query" {
        return Err("expected write, dump, or query".into());
    }
    for line in io::stdin().lock().lines() {
        let line = line?;
        let mut fields = line.split_whitespace();
        match fields.next().ok_or("empty query")? {
            "G" => {
                let key = u64::from_str_radix(fields.next().ok_or("missing key")?, 16)?;
                match file.get(&key) {
                    Some(value) => println!("{value:016x}"),
                    None => println!("-"),
                }
            }
            "R" => {
                let lo = u64::from_str_radix(fields.next().ok_or("missing lower bound")?, 16)?;
                let hi = u64::from_str_radix(fields.next().ok_or("missing upper bound")?, 16)?;
                for (k, v) in file.range(lo..hi) {
                    print!("{k:016x}:{v:016x},");
                }
                println!();
            }
            "L" => println!("{}", file.len()),
            _ => return Err("unknown query".into()),
        }
    }
    Ok(())
}
