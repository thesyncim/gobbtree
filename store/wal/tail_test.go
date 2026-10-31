package wal

import (
	"bytes"
	"math/rand/v2"
	"slices"
	"testing"
)

// The specs upstream's Verus proves of the tail (wal/tail.rs, lemmas.rs),
// as properties: bytes past a tail scan to records exactly when they hold
// them, which are concatenations of whole appends with LSNs one after
// another; an append's bytes hold its records; the scan and the append give
// the same addresses and the same tail; and a scan picks up where it
// stopped (lemma_scan_in_two).

// headerBytes is upstream's WalTail::header_bytes.
func headerBytes(t tail) []byte {
	if t.fileLen == 0 {
		return fileHeader[:]
	}
	return nil
}

// concatBytes is upstream's StoredRecord::concat_bytes.
func concatBytes(records []storedRecord) []byte {
	var b []byte
	for _, r := range records {
		b = append(b, specBytes(r)...)
	}
	return b
}

// holds is upstream's WalTail::holds: b, the last file's bytes past t, is
// the header where the file starts, then the records' bytes, their LSNs one
// after another from t's, the last ending an append.
func holds(t tail, b []byte, records []storedRecord) bool {
	for i, r := range records {
		if r.lsn != t.nextLSN+uint64(i) {
			return false
		}
	}
	if len(records) > 0 && !records[len(records)-1].lastInAppend {
		return false
	}
	return bytes.Equal(b, append(slices.Clone(headerBytes(t)), concatBytes(records)...))
}

// addresses is upstream's WalTail::addresses, each record's address_at: t's
// file, the offset past t, the header and the records before it, and the
// record's length.
func addresses(t tail, records []storedRecord) []Address {
	var a []Address
	offset := t.fileLen + uint64(len(headerBytes(t)))
	for _, r := range records {
		length := uint64(len(specBytes(r)))
		a = append(a, Address{file: t.file, offset: offset, length: length})
		offset += length
	}
	return a
}

// appendEnds is upstream's WalTail::append_ends.
func appendEnds(records []storedRecord) uint64 {
	var n uint64
	for _, r := range records {
		if r.lastInAppend {
			n++
		}
	}
	return n
}

// after is upstream's WalTail::after.
func after(t tail, records []storedRecord) tail {
	return tail{
		file:        t.file,
		fileLen:     t.fileLen + uint64(len(headerBytes(t))+len(concatBytes(records))),
		fileAppends: t.fileAppends + appendEnds(records),
		nextLSN:     t.nextLSN + uint64(len(records)),
	}
}

// appendRecords is upstream's EncodedAppend::records: the records an append
// of parts at position holds.
func appendRecords(position tail, parts []part) []storedRecord {
	records := make([]storedRecord, len(parts))
	for i, p := range parts {
		records[i] = storedRecord{
			lsn:          position.nextLSN + uint64(i),
			kind:         p.kind,
			lastInAppend: i == len(parts)-1,
			key:          p.key,
			value:        p.value,
		}
	}
	return records
}

// heldRecords is the records b holds past t, if it holds any. Records'
// bytes split into records one way only (upstream's lemma_concat_unique),
// so the one run that can hold is read off b a record at a time.
func heldRecords(t tail, b []byte) ([]storedRecord, bool) {
	rest, ok := bytes.CutPrefix(b, headerBytes(t))
	if !ok {
		return nil, false
	}
	var records []storedRecord
	for len(rest) > 0 {
		r, ok := readFrom(rest)
		if !ok || !bytes.HasPrefix(rest, specBytes(r)) {
			return nil, false
		}
		records = append(records, r)
		rest = rest[len(specBytes(r)):]
	}
	return records, holds(t, b, records)
}

func sameRecords(a, b []storedRecord) bool {
	return slices.EqualFunc(a, b, sameRecord)
}

// checkScan checks scanBytes's spec on b past t: it scans records exactly
// when b holds some, those, at their addresses, to the tail after them.
func checkScan(tb testing.TB, t tail, b []byte) scannedBytes {
	tb.Helper()
	scanned, stop, ok := t.scanBytes(b)
	held, holdsAny := heldRecords(t, b)
	if ok != holdsAny {
		tb.Fatalf("scanned %v, stopping at %d, but the bytes hold records: %v\ntail %+v\n%x", ok, stop, holdsAny, t, b)
	}
	if !ok {
		if stop < 0 || stop > len(b) {
			tb.Fatalf("stopped at %d of %d bytes", stop, len(b))
		}
		return scanned
	}
	if !holds(t, b, scanned.records) || !sameRecords(scanned.records, held) {
		tb.Fatalf("scanned %+v, which the bytes don't hold; they hold %+v", scanned.records, held)
	}
	if want := addresses(t, scanned.records); !slices.Equal(scanned.addresses, want) {
		tb.Fatalf("scanned at %v, want %v", scanned.addresses, want)
	}
	if want := after(t, scanned.records); scanned.tail != want {
		tb.Fatalf("scanned to %+v, want %+v", scanned.tail, want)
	}
	return scanned
}

// checkEncodedAppend checks newEncodedAppend's spec, and that its bytes
// scan back: they hold its records at position, at its addresses, to its
// tail, and a scan reads those records there, at those addresses, to that
// tail.
func checkEncodedAppend(tb testing.TB, position tail, parts []part) encodedAppend {
	tb.Helper()
	a := newEncodedAppend(position, parts)
	records := appendRecords(position, parts)
	if a.file != position.file || a.offset != position.fileLen {
		tb.Fatalf("append at file %d offset %d, want %+v", a.file, a.offset, position)
	}
	if !holds(position, a.bytes, records) {
		tb.Fatalf("append's bytes don't hold its records\n%x", a.bytes)
	}
	if want := addresses(position, records); !slices.Equal(a.addresses, want) {
		tb.Fatalf("appended at %v, want %v", a.addresses, want)
	}
	if want := after(position, records); a.tail != want {
		tb.Fatalf("appended to %+v, want %+v", a.tail, want)
	}
	scanned := checkScan(tb, position, a.bytes)
	if !sameRecords(scanned.records, records) || !slices.Equal(scanned.addresses, a.addresses) || scanned.tail != a.tail {
		tb.Fatalf("scanned %+v at %v to %+v, appended %+v at %v to %+v",
			scanned.records, scanned.addresses, scanned.tail, records, a.addresses, a.tail)
	}
	return a
}

// checkScanInTwo checks upstream's lemma_scan_in_two on b, which holds
// records past t, and more, which holds moreRecords past the tail after
// them: both hold all of them, at the same addresses, to the same tail; and
// scanning both at once reads what scanning each, one after the other,
// does.
func checkScanInTwo(tb testing.TB, t tail, b []byte, records []storedRecord, more []byte, moreRecords []storedRecord) {
	tb.Helper()
	between := after(t, records)
	if !holds(t, b, records) || !holds(between, more, moreRecords) {
		tb.Fatal("lemma_scan_in_two's precondition fails")
	}
	all := append(slices.Clone(records), moreRecords...)
	both := append(slices.Clone(b), more...)
	if !holds(t, both, all) {
		tb.Fatal("both don't hold all")
	}
	if !slices.Equal(addresses(t, all), append(addresses(t, records), addresses(between, moreRecords)...)) {
		tb.Fatal("all's addresses aren't the first's then the rest's")
	}
	if after(t, all) != after(between, moreRecords) {
		tb.Fatal("the tail after all isn't the tail after the rest")
	}
	first, _, ok1 := t.scanBytes(b)
	second, _, ok2 := first.tail.scanBytes(more)
	whole, _, ok := t.scanBytes(both)
	if !ok1 || !ok2 || !ok {
		tb.Fatalf("scans %v %v %v", ok1, ok2, ok)
	}
	if !sameRecords(whole.records, append(slices.Clone(first.records), second.records...)) ||
		!slices.Equal(whole.addresses, append(slices.Clone(first.addresses), second.addresses...)) ||
		whole.tail != second.tail {
		tb.Fatalf("scanned whole %+v at %v to %+v; in two %+v then %+v",
			whole.records, whole.addresses, whole.tail, first, second)
	}
}

// walFile is appends encoded one after another past a tail, each checked
// against newEncodedAppend's spec: the header where the file starts, which
// the first append's bytes hold, then their records' bytes.
type walFile struct {
	bytes []byte
	// ends is where the header ends in bytes, then where each append does.
	ends []int
	// records is each append's records.
	records [][]storedRecord
	// tails is the tail after the header, then after each append.
	tails []tail
}

func encodeAppends(tb testing.TB, position tail, appends [][]part) walFile {
	tb.Helper()
	f := walFile{bytes: slices.Clone(headerBytes(position)), tails: []tail{after(position, nil)}}
	f.ends = []int{len(f.bytes)}
	for _, parts := range appends {
		a := checkEncodedAppend(tb, position, parts)
		f.bytes = append(f.bytes, a.bytes[len(headerBytes(position)):]...)
		f.ends = append(f.ends, len(f.bytes))
		f.records = append(f.records, appendRecords(position, parts))
		position = a.tail
		f.tails = append(f.tails, position)
	}
	return f
}

// checkWALFile checks the scan on f past t: whole, in two at each append's
// end, changed at a place m picks, with an append m picks repeated or
// dropped, and past a tail of another LSN. The appends' records are whole,
// so only their LSNs fail those last checks.
func checkWALFile(tb testing.TB, t tail, f walFile, m uint64) {
	tb.Helper()
	whole := checkScan(tb, t, f.bytes)
	if want := slices.Concat(f.records...); !sameRecords(whole.records, want) || whole.tail != f.tails[len(f.tails)-1] {
		tb.Fatalf("scanned %+v to %+v, appended %+v to %+v", whole.records, whole.tail, want, f.tails[len(f.tails)-1])
	}
	for i, end := range f.ends {
		checkScanInTwo(tb, t, f.bytes[:end], slices.Concat(f.records[:i]...), f.bytes[end:], slices.Concat(f.records[i:]...))
	}
	checkScan(tb, t, mutate(f.bytes, m))
	if appends := len(f.ends) - 1; appends > 0 {
		i := int(m % uint64(appends))
		start, end := f.ends[i], f.ends[i+1]
		checkScan(tb, t, slices.Concat(f.bytes[:end], f.bytes[start:end], f.bytes[end:]))
		checkScan(tb, t, slices.Concat(f.bytes[:start], f.bytes[end:]))
		other := t
		other.nextLSN += 1 + m>>32
		checkScan(tb, other, f.bytes)
	}
}

func randomTail(rng *rand.Rand) tail {
	t := tail{file: rng.Uint64N(4), nextLSN: rng.Uint64N(1 << 40)}
	if rng.IntN(2) == 0 {
		t.fileLen, t.fileAppends = 12+rng.Uint64N(1<<30), rng.Uint64N(10_000)
	}
	return t
}

func randomAppends(rng *rand.Rand) [][]part {
	appends := make([][]part, rng.IntN(8))
	for i := range appends {
		for range 1 + rng.IntN(4) {
			appends[i] = append(appends[i], part{
				kind:  specKinds[rng.IntN(len(specKinds))],
				key:   randomBytes(rng, randomLength(rng)),
				value: randomBytes(rng, randomLength(rng)),
			})
		}
	}
	return appends
}

// An append's bytes hold its records, and scan back to them, at its
// addresses, to its tail.
func TestEncodedAppendScansBack(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	for range 3000 {
		appends := randomAppends(rng)
		if len(appends) > 0 {
			checkEncodedAppend(t, randomTail(rng), appends[0])
		}
	}
}

// A scan reads exactly the concatenations of whole appends, their LSNs one
// after another: of appends, and of their bytes changed.
func TestScanReadsExactlyWholeAppends(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	for range 1000 {
		position := randomTail(rng)
		f := encodeAppends(t, position, randomAppends(rng))
		for range 10 {
			checkScan(t, position, mutate(f.bytes, rng.Uint64()))
		}
		// Bytes cut at each record's end: whole appends only where an append
		// ends.
		for _, a := range addresses(position, slices.Concat(f.records...)) {
			checkScan(t, position, f.bytes[:a.offset-position.fileLen+a.length])
		}
		checkScan(t, position, randomBytes(rng, rng.IntN(64)))
	}
}

// Upstream's lemma_scan_in_two (wal/lemmas.rs): a scan picks up where it
// stopped.
func TestScanInTwo(t *testing.T) {
	rng := rand.New(rand.NewPCG(9, 10))
	for range 1000 {
		position := randomTail(rng)
		checkWALFile(t, position, encodeAppends(t, position, randomAppends(rng)), rng.Uint64())
	}
}

// appendsOf reads appends from recipe, a byte a part: its bits 0 and 1 the
// kind, 3 an insert; 2 to 4 the key's length; 5 and 6 the value's, of 0, 1,
// 7 or 40 bytes; and 7 whether the part ends its append. The key's and the
// value's bytes are the recipe's next, as many as there are.
func appendsOf(recipe []byte) [][]part {
	var appends [][]part
	var parts []part
	for len(recipe) > 0 {
		c := recipe[0]
		recipe = recipe[1:]
		take := func(n int) []byte {
			n = min(n, len(recipe))
			b := recipe[:n:n]
			recipe = recipe[n:]
			return b
		}
		p := part{kind: []kind{kindInsert, kindRemove, kindClaim, kindInsert}[c&3]}
		p.key = take(int(c >> 2 & 7))
		p.value = take([]int{0, 1, 7, 40}[c>>5&3])
		parts = append(parts, p)
		if c&0x80 != 0 {
			appends, parts = append(appends, parts), nil
		}
	}
	if len(parts) > 0 {
		appends = append(appends, parts)
	}
	return appends
}

// tailOf is a tail past which to scan: where a file starts, or past its
// header.
func tailOf(seed uint64) tail {
	t := tail{file: seed & 3, nextLSN: seed >> 24}
	if seed&4 != 0 {
		t.fileLen, t.fileAppends = 12+(seed>>3&0xffff), seed>>19&0x1f
	}
	return t
}

func FuzzScan(f *testing.F) {
	f.Add([]byte("\x85key\x01\x02value!\x80"), uint64(0), uint64(0))
	f.Add([]byte("\x05abc\x26de\x7fxy\x87"), uint64(5<<24|4), uint64(3<<8|2))
	f.Add([]byte("\x00\x01\x02\x83\xe4keyvaluevaluevaluevaluevaluevaluevaluevalue"), uint64(1<<24|7), uint64(17<<8|4))
	f.Fuzz(func(t *testing.T, recipe []byte, seed, m uint64) {
		// Scanning in two at each append's end is quadratic in the appends.
		recipe = recipe[:min(len(recipe), 512)]
		position := tailOf(seed)
		checkWALFile(t, position, encodeAppends(t, position, appendsOf(recipe)), m)
		checkScan(t, position, recipe)
		checkScan(t, position, append(slices.Clone(fileHeader[:]), recipe...))
	})
}
