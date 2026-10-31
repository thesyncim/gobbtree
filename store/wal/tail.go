package wal

// The WAL's tail, upstream's wal/tail.rs: where the WAL ends, as the writer
// knows it. A scan moves it over the last file's bytes (scanBytes); an
// append goes after it, in the bytes newEncodedAppend encodes.
//
// Upstream's Verus proves that a scan reads exactly what appends write:
// bytes past a tail scan to records exactly when they hold them
// (WalTail::holds), and an append's bytes hold its records; the scan and
// the append give the same addresses for them, and the same tail after
// them; and a scan picks up where it stopped (lemma_scan_in_two). Here the
// same specs are property tests and fuzz targets (tail_test.go).

// fileHeaderLen is a WAL file's header's length.
const fileHeaderLen = 12

// fileHeader is a WAL file's header: the magic, then the format's version,
// 1, a little-endian uint32. Written once, by the append that creates the
// file; a file of another magic or version is refused, never misread.
var fileHeader = [fileHeaderLen]byte{'b', 'b', 't', 'r', 'e', 'e', 'W', 'L', 1, 0, 0, 0}

// tail is where the WAL ends, as the writer knows it, upstream's WalTail.
// The zero tail is a WAL's without files, upstream's WalTail::empty.
type tail struct {
	// file is the last WAL file's number.
	file uint64
	// fileLen is its length: 0 while it doesn't exist.
	fileLen uint64
	// fileAppends is the appends it holds.
	fileAppends uint64
	// nextLSN is the LSN of the WAL's next record.
	nextLSN uint64
}

// nextFile is the tail at the start of the next file.
func (t tail) nextFile() tail { return tail{file: t.file + 1, nextLSN: t.nextLSN} }

// appendPosition is where the next append goes: here, or at the start of
// the next file once this one holds maxAppends.
func (t tail) appendPosition(maxAppends uint32) tail {
	if t.fileAppends == uint64(maxAppends) {
		return t.nextFile()
	}
	return t
}

// scannedBytes is what a scan read: records, their addresses, and the tail
// after them.
type scannedBytes struct {
	records   []storedRecord
	addresses []Address
	tail      tail
}

// scanBytes scans b, the last WAL file's bytes past t: the records they
// hold, with their addresses and the tail after them; else, ok false, where
// they stop holding records. It scans records only if b holds them: the
// header where the file starts, then the records' bytes, their LSNs one
// after another from t's, the last ending an append; and every run of
// records b holds. The records alias b.
func (t tail) scanBytes(b []byte) (scanned scannedBytes, stop int, ok bool) {
	pos := 0
	if t.fileLen == 0 {
		for i := range fileHeaderLen {
			if i == len(b) || b[i] != fileHeader[i] {
				return scannedBytes{}, 0, false
			}
		}
		pos = fileHeaderLen
	}
	var records []storedRecord
	var addresses []Address
	// Where the last append scanned whole ends.
	lastAppendEnd := pos
	nextLSN, fileAppends := t.nextLSN, t.fileAppends
	for pos < len(b) {
		r, ok := decodeRecord(b[pos:])
		if !ok || r.lsn != nextLSN {
			return scannedBytes{}, pos, false
		}
		n := r.encodedLen()
		addresses = append(addresses, Address{file: t.file, offset: t.fileLen + uint64(pos), length: uint64(n)})
		pos += n
		nextLSN++
		if r.lastInAppend {
			fileAppends++
			lastAppendEnd = pos
		}
		records = append(records, r)
	}
	if lastAppendEnd != pos {
		return scannedBytes{}, lastAppendEnd, false
	}
	after := tail{file: t.file, fileLen: t.fileLen + uint64(len(b)), fileAppends: fileAppends, nextLSN: nextLSN}
	return scannedBytes{records: records, addresses: addresses, tail: after}, 0, true
}

// encodedAppend is one append's bytes, the addresses of its records, and
// the tail after it, upstream's EncodedAppend.
type encodedAppend struct {
	file, offset uint64
	bytes        []byte
	addresses    []Address
	tail         tail
}

// newEncodedAppend encodes parts, at least one, each a record's kind, key
// and value, as an append at position: bytes that hold its records there,
// their LSNs on from position's and the last ending the append, their
// addresses, and the tail after them. Keys and values must be at most
// math.MaxUint32 bytes each.
func newEncodedAppend(position tail, parts []part) encodedAppend {
	var b []byte
	if position.fileLen == 0 {
		b = append(b, fileHeader[:]...)
	}
	addresses := make([]Address, 0, len(parts))
	for i, p := range parts {
		pos := len(b)
		r := storedRecord{
			lsn:          position.nextLSN + uint64(i),
			kind:         p.kind,
			lastInAppend: i+1 == len(parts),
			key:          p.key,
			value:        p.value,
		}
		b = r.encode(b)
		addresses = append(addresses, Address{file: position.file, offset: position.fileLen + uint64(pos), length: uint64(len(b) - pos)})
	}
	after := tail{
		file:        position.file,
		fileLen:     position.fileLen + uint64(len(b)),
		fileAppends: position.fileAppends + 1,
		nextLSN:     position.nextLSN + uint64(len(parts)),
	}
	return encodedAppend{file: position.file, offset: position.fileLen, bytes: b, addresses: addresses, tail: after}
}
