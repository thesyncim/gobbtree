use bbtree::file::InWriter;
use bbtree_mmap::{BBTreeMapFile, DiskInput, DiskType};
use std::io::{self, BufRead};

#[derive(Clone, Copy, PartialEq, Eq)]
#[repr(transparent)]
struct ThreeWords([u64; 3]);
// Exactly three plain integers, with no padding, pointers or invalid bit patterns.
unsafe impl bbtree::file::PlainBytes for ThreeWords {}
bbtree_mmap::plain_disk_type!(ThreeWords);

// Go's int and uint (and uintptr), as Rust's isize and usize: plain keys
// without inline keys under Auto, which recognizes only u64, i64, u128 and
// i128. The orphan rule keeps plain_disk_type! off the std types, so they
// are wrapped; the layout and fingerprint are the std types'.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
#[repr(transparent)]
struct USize(usize);
// A plain integer.
unsafe impl bbtree::file::PlainBytes for USize {}
bbtree_mmap::plain_disk_type!(USize);

#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
#[repr(transparent)]
struct ISize(isize);
// A plain integer.
unsafe impl bbtree::file::PlainBytes for ISize {}
bbtree_mmap::plain_disk_type!(ISize);

/// A key or value as the Go side encodes it in an entries file: a plain
/// type as its native-endian bytes, bytes as a native-endian u32 length and
/// then the bytes.
trait Decode: Sized {
    fn decode(input: &mut &[u8]) -> Self;
}

fn take<'a>(input: &mut &'a [u8], n: usize) -> &'a [u8] {
    let (head, rest) = input.split_at(n);
    *input = rest;
    head
}

macro_rules! decode_plain {
    ($($plain:ty),*) => {
        $(impl Decode for $plain {
            fn decode(input: &mut &[u8]) -> Self {
                <$plain>::from_ne_bytes(take(input, size_of::<$plain>()).try_into().unwrap())
            }
        })*
    };
}

decode_plain!(u8, u16, u32, u64, i8, i16, i32, i64, f32, f64);

impl<const N: usize> Decode for [u8; N] {
    fn decode(input: &mut &[u8]) -> Self {
        take(input, N).try_into().unwrap()
    }
}

impl Decode for Vec<u8> {
    fn decode(input: &mut &[u8]) -> Self {
        let len = u32::decode(input) as usize;
        take(input, len).to_vec()
    }
}

impl Decode for ThreeWords {
    fn decode(input: &mut &[u8]) -> Self {
        ThreeWords([u64::decode(input), u64::decode(input), u64::decode(input)])
    }
}

impl Decode for USize {
    fn decode(input: &mut &[u8]) -> Self {
        USize(u64::decode(input) as usize)
    }
}

impl Decode for ISize {
    fn decode(input: &mut &[u8]) -> Self {
        ISize(i64::decode(input) as isize)
    }
}

/// Writes the map of the entries in `input`, a count and then each key and
/// value, to `path`, as BBTreeMapFile::create does.
fn write_entries<K, V>(mut input: &[u8], path: &str) -> Result<(), Box<dyn std::error::Error>>
where
    K: DiskType<Stored: Ord> + Decode + DiskInput<K::Stored>,
    V: DiskType + Decode + DiskInput<V::Stored>,
    InWriter<K::Stored>: Ord,
{
    let count = u64::decode(&mut input) as usize;
    let mut entries = Vec::with_capacity(count);
    for _ in 0..count {
        let key = K::decode(&mut input);
        let value = V::decode(&mut input);
        entries.push((key, value));
    }
    if !input.is_empty() {
        return Err("trailing bytes after the entries".into());
    }
    BBTreeMapFile::<K, V>::create(path, entries)?;
    Ok(())
}

/// `write_entries` for the key and value types the Go test names.
macro_rules! dispatch_entries {
    ($key:expr, $value:expr, $input:expr, $path:expr; $(($k:literal, $kt:ty, $v:literal, $vt:ty)),* $(,)?) => {
        match ($key, $value) {
            $(($k, $v) => write_entries::<$kt, $vt>($input, $path),)*
            _ => Err(format!("unsupported key and value types {} {}", $key, $value).into()),
        }
    };
}

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
    if command == "write-entries" {
        // write-entries KEY VALUE ENTRIES OUT: the Go test's byte-identity
        // gate, which compares OUT with what Go writes for the same entries.
        let (key, value) = (args.get(2).ok_or("missing key type")?, args.get(3).ok_or("missing value type")?);
        let input = std::fs::read(args.get(4).ok_or("missing entries file")?)?;
        let out = args.get(5).ok_or("missing output path")?;
        return dispatch_entries!(key.as_str(), value.as_str(), &input, out;
            ("u64", u64, "u64", u64),
            ("i64", i64, "i64", i64),
            ("u32", u32, "u32", u32),
            ("i32", i32, "b13", [u8; 13]),
            ("u16", u16, "u8", u8),
            ("i16", i16, "f32", f32),
            ("u8", u8, "i8", i8),
            ("i8", i8, "u64", u64),
            ("usize", USize, "u64", u64),
            ("isize", ISize, "i64", i64),
            ("bytes", Vec<u8>, "u64", u64),
            ("u64", u64, "bytes", Vec<u8>),
            ("bytes", Vec<u8>, "bytes", Vec<u8>),
            ("u64", u64, "b0", [u8; 0]),
            ("bytes", Vec<u8>, "b0", [u8; 0]),
            ("bytes", Vec<u8>, "w3", ThreeWords),
            ("u64", u64, "f64", f64),
            ("i64", i64, "b16", [u8; 16]),
            ("u32", u32, "bytes", Vec<u8>),
            ("isize", ISize, "bytes", Vec<u8>),
        );
    }
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
