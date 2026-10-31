package wal

import (
	"context"
	"encoding/binary"
	"hash/crc32"
)

// Records, upstream's wal/record.rs: the caller's (Record), and as the WAL
// stores them (storedRecord), which the writer encodes, and the scan and
// lookups (Address.ReadValue) decode and check:
//
//	| crc: u32 | LSN: u64 | key length: u32 | value length: u32 | kind: u8 | key … | value … |
//
// little-endian and unaligned, the CRC32C of every byte after it. The kind
// is an insert, a removal or a claim, plus 0x80 on an append's last record.
//
// Upstream's Verus proves that encoding round-trips: a record decodes from
// its bytes, whatever follows them, and nothing else does. Here the same
// specs are property tests and fuzz targets (record_test.go).

// Record is one key's insert or removal, upstream's Record. Insert and
// Remove make them; the zero Record inserts an empty value at an empty key.
type Record struct {
	key, value []byte
	remove     bool
}

// Insert is an insert of value at key, upstream's Record::Insert.
func Insert(key, value []byte) Record { return Record{key: key, value: value} }

// Remove is a removal of key, upstream's Record::Remove: it has no value.
func Remove(key []byte) Record { return Record{key: key, remove: true} }

// IsRemove reports whether r is a removal.
func (r Record) IsRemove() bool { return r.remove }

// Key is the key r inserts at or removes.
func (r Record) Key() []byte { return r.key }

// Value is the value r inserts; nil for a removal.
func (r Record) Value() []byte { return r.value }

// part is a record as an append encodes it, its kind, key and value:
// upstream's (Kind, &[u8], &[u8]).
type part struct {
	kind       kind
	key, value []byte
}

// part is r's kind, key and value, upstream's Record::kind_key_value.
func (r Record) part() part {
	if r.remove {
		return part{kind: kindRemove, key: r.key}
	}
	return part{kind: kindInsert, key: r.key, value: r.value}
}

// Address is where a record lies: its WAL file, its offset in it, and its
// length, upstream's RecordAddress. The index keeps it, to read the record
// back. Addresses are equal under == when they are one record's.
type Address struct {
	file, offset, length uint64
}

// ReadValue reads the insert at a from storage, and gives its value: one
// Read of the record, whose CRC, length and kind it checks, as upstream's
// RecordAddress::read_value. A removal's address fails with a
// *CorruptedError, having no value.
func (a Address) ReadValue(ctx context.Context, storage Storage) ([]byte, error) {
	// make panics for a record longer than memory, as upstream's expect.
	b := make([]byte, a.length)
	if err := storage.Read(ctx, a.file, a.offset, b); err != nil {
		return nil, &StorageError{Err: err}
	}
	r, ok := decodeRecord(b)
	if !ok || r.encodedLen() != len(b) || r.kind != kindInsert {
		return nil, &CorruptedError{File: a.file, Offset: a.offset}
	}
	// The value ends the record: upstream drains the bytes before it.
	return r.value, nil
}

// recordHeaderLen is a record's bytes before its key: CRC, LSN, both
// lengths and kind.
const recordHeaderLen = 21

// kind is a record's kind, upstream's Kind. Its values are its kind bytes
// on a record that doesn't end its append.
type kind uint8

const (
	kindInsert kind = 1
	kindRemove kind = 2
	// kindClaim is a writer's claim on the WAL, its key the writer's id.
	kindClaim kind = 3
)

// lastInAppend is the kind byte's bit on an append's last record.
const lastInAppend = 0x80

// kindByte is the kind byte of a record of kind k: 1, 2 or 3, plus 0x80 on
// an append's last record, upstream's Kind::byte.
func (k kind) kindByte(last bool) uint8 {
	if last {
		return uint8(k) + lastInAppend
	}
	return uint8(k)
}

// kindOfByte is the kind of a kind byte, and whether its record is its
// append's last, upstream's Kind::of_byte. Each of the six kind bytes is one
// kind's, and no other byte is.
func kindOfByte(b uint8) (k kind, last, ok bool) {
	switch b {
	case 1, 2, 3:
		return kind(b), false, true
	case 0x81, 0x82, 0x83:
		return kind(b - lastInAppend), true, true
	}
	return 0, false, false
}

// castagnoli is CRC32C's table, upstream's crc32c crate's polynomial;
// hash/crc32 computes it with the SSE 4.2 and ARMv8 CRC instructions where
// the processor has them.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// storedRecord is a record as the WAL stores it, upstream's StoredRecord.
type storedRecord struct {
	lsn          uint64
	kind         kind
	lastInAppend bool
	key, value   []byte
}

// encode appends the record's bytes to out. Its key and value must be at
// most math.MaxUint32 bytes each, the uint32s its bytes keep their lengths
// in, which the writer checks.
func (r *storedRecord) encode(out []byte) []byte {
	start := len(out)
	// A zero CRC, written over once the CRC of the bytes after it is known.
	out = binary.LittleEndian.AppendUint32(out, 0)
	out = binary.LittleEndian.AppendUint64(out, r.lsn)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(r.key)))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(r.value)))
	out = append(out, r.kind.kindByte(r.lastInAppend))
	out = append(out, r.key...)
	out = append(out, r.value...)
	binary.LittleEndian.PutUint32(out[start:], crc32.Checksum(out[start+4:], castagnoli))
	return out
}

// decodeRecord is the record at the start of b, if one is: it decodes a
// record only if b starts with its bytes, and every record b starts with,
// as upstream's StoredRecord::decode proves. Its key and value alias b,
// their capacities cut to their lengths.
func decodeRecord(b []byte) (storedRecord, bool) {
	if len(b) < recordHeaderLen {
		return storedRecord{}, false
	}
	crc := binary.LittleEndian.Uint32(b[0:4])
	lsn := binary.LittleEndian.Uint64(b[4:12])
	keyLen := uint64(binary.LittleEndian.Uint32(b[12:16]))
	valueLen := uint64(binary.LittleEndian.Uint32(b[16:20]))
	k, last, ok := kindOfByte(b[20])
	if !ok || keyLen > uint64(len(b)-recordHeaderLen) {
		return storedRecord{}, false
	}
	keyEnd := recordHeaderLen + int(keyLen)
	if valueLen > uint64(len(b)-keyEnd) {
		return storedRecord{}, false
	}
	n := keyEnd + int(valueLen)
	if crc32.Checksum(b[4:n], castagnoli) != crc {
		return storedRecord{}, false
	}
	return storedRecord{
		lsn:          lsn,
		kind:         k,
		lastInAppend: last,
		key:          b[recordHeaderLen:keyEnd:keyEnd],
		value:        b[keyEnd:n:n],
	}, true
}

// encodedLen is the record's length in bytes, upstream's StoredRecord::len.
func (r *storedRecord) encodedLen() int { return recordHeaderLen + len(r.key) + len(r.value) }
