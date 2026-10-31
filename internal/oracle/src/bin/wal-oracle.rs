//! bbtree-store's WAL at the pinned revision, on WAL files in a directory,
//! for store/wal's byte-compatibility test (TestUpstreamWALOracle).
//!
//! - `wal-oracle write DIR MAX_APPENDS` runs the script on stdin: `open`
//!   opens the WAL, claiming it, and prints `opened`; `append I:KEY:VALUE
//!   R:KEY …` appends inserts and removals, keys and values in hex, as one
//!   append, and prints their addresses.
//! - `wal-oracle dump DIR MAX_APPENDS` opens the WAL, printing each record it
//!   replays with its address, then `claimed` or the open's error.
use std::error::Error;
use std::fs::{self, File, OpenOptions};
use std::io::{self, BufRead, Write};
use std::os::unix::fs::FileExt;
use std::path::PathBuf;

use bbtree_store::wal::{AppendError, Record, WalError, WalStorage, WalWriter};
use pollster::block_on;

/// WAL files in a directory, a file each, named by number in 16 hex
/// digits. An append writes at the file's end, unsynced: the oracle's files
/// needn't outlive a crash.
#[derive(Clone)]
struct DirStorage<const MAX_APPENDS: u32> {
    dir: PathBuf,
}

impl<const MAX_APPENDS: u32> DirStorage<MAX_APPENDS> {
    fn path(&self, file: u64) -> PathBuf {
        self.dir.join(format!("{file:016x}"))
    }
}

impl<const MAX_APPENDS: u32> WalStorage for DirStorage<MAX_APPENDS> {
    const MAX_APPENDS: u32 = MAX_APPENDS;

    async fn append(&self, file: u64, offset: u64, bytes: &[u8]) -> Result<(), AppendError> {
        let mut options = OpenOptions::new();
        if offset == 0 {
            options.write(true).create_new(true);
        } else {
            options.append(true);
        }
        let mut stored = match options.open(self.path(file)) {
            Ok(stored) => stored,
            Err(error)
                if error.kind() == io::ErrorKind::AlreadyExists
                    || (offset != 0 && error.kind() == io::ErrorKind::NotFound) =>
            {
                return Err(AppendError::OffsetMismatch);
            }
            Err(error) => return Err(AppendError::OutcomeUnknown(error)),
        };
        if offset != 0 && stored.metadata().map_err(AppendError::OutcomeUnknown)?.len() != offset {
            return Err(AppendError::OffsetMismatch);
        }
        stored.write_all(bytes).map_err(AppendError::OutcomeUnknown)
    }

    async fn len(&self, file: u64) -> io::Result<Option<u64>> {
        match fs::metadata(self.path(file)) {
            Ok(metadata) => Ok(Some(metadata.len())),
            Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(None),
            Err(error) => Err(error),
        }
    }

    async fn read(&self, file: u64, offset: u64, buf: &mut [u8]) -> io::Result<()> {
        File::open(self.path(file))?.read_exact_at(buf, offset)
    }
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

fn unhex(text: &str) -> Result<Vec<u8>, Box<dyn Error>> {
    if text.len() % 2 != 0 {
        return Err(format!("odd hex {text:?}").into());
    }
    (0..text.len())
        .step_by(2)
        .map(|i| Ok(u8::from_str_radix(&text[i..i + 2], 16)?))
        .collect()
}

/// A record of the script: whether it removes, its key and its value.
fn parse_record(token: &str) -> Result<(bool, Vec<u8>, Vec<u8>), Box<dyn Error>> {
    let fields: Vec<&str> = token.split(':').collect();
    match fields[..] {
        ["I", key, value] => Ok((false, unhex(key)?, unhex(value)?)),
        ["R", key] => Ok((true, unhex(key)?, Vec::new())),
        _ => Err(format!("unknown record {token:?}").into()),
    }
}

fn write<S: WalStorage + Clone>(storage: S) -> Result<(), Box<dyn Error>> {
    let mut writer = None;
    for line in io::stdin().lock().lines() {
        let line = line?;
        let mut tokens = line.split_whitespace();
        match tokens.next() {
            Some("open") => {
                writer = Some(block_on(WalWriter::open(storage.clone(), |_, _| {}))?);
                println!("opened");
            }
            Some("append") => {
                let parts = tokens.map(parse_record).collect::<Result<Vec<_>, _>>()?;
                let records: Vec<Record<'_>> = parts
                    .iter()
                    .map(|(remove, key, value)| match remove {
                        true => Record::Remove { key },
                        false => Record::Insert { key, value },
                    })
                    .collect();
                let writer = writer.as_mut().ok_or("append before open")?;
                let addresses = block_on(writer.append(&records))?;
                let addresses: Vec<String> = addresses.iter().map(|a| format!("{a:?}")).collect();
                println!("{}", addresses.join(" "));
            }
            _ => return Err(format!("unknown line {line:?}").into()),
        }
    }
    Ok(())
}

fn dump<S: WalStorage>(storage: S) {
    let opened = block_on(WalWriter::open(storage, |address, record| match record {
        Record::Insert { key, value } => {
            println!("I {address:?} key={} value={}", hex(key), hex(value))
        }
        Record::Remove { key } => println!("R {address:?} key={}", hex(key)),
    }));
    match opened {
        Ok(_) => println!("claimed"),
        Err(WalError::Corrupted { file, offset }) => println!("corrupted {file} {offset}"),
        Err(WalError::TailMoved) => println!("tail moved"),
        Err(WalError::Storage(error)) => println!("storage {error}"),
    }
}

fn run<S: WalStorage + Clone>(command: &str, storage: S) -> Result<(), Box<dyn Error>> {
    match command {
        "write" => write(storage),
        "dump" => {
            dump(storage);
            Ok(())
        }
        _ => Err(format!("unknown command {command:?}").into()),
    }
}

fn main() -> Result<(), Box<dyn Error>> {
    let args: Vec<String> = std::env::args().collect();
    let [_, command, dir, max_appends] = &args[..] else {
        return Err("usage: wal-oracle write|dump DIR MAX_APPENDS".into());
    };
    let dir = PathBuf::from(dir);
    // MAX_APPENDS is a constant of the storage's type: one type each.
    match max_appends.as_str() {
        "1" => run(command, DirStorage::<1> { dir }),
        "2" => run(command, DirStorage::<2> { dir }),
        "3" => run(command, DirStorage::<3> { dir }),
        "10000" => run(command, DirStorage::<10_000> { dir }),
        _ => Err(format!("MAX_APPENDS is 1, 2, 3 or 10000, not {max_appends}").into()),
    }
}
