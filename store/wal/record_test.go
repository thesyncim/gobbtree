package wal

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"math"
	"math/rand/v2"
	"testing"
)

// The specs upstream's Verus proves of records (wal/record.rs, lemmas.rs),
// as properties: encoding round-trips, a record decodes from its bytes
// whatever follows them, and nothing else decodes. The spec functions here
// build bytes as the specs read, apart from the code's.

// specKinds is every kind.
var specKinds = []kind{kindInsert, kindRemove, kindClaim}

// specKindByte is upstream's Kind::spec_byte.
func specKindByte(k kind, last bool) uint8 {
	b := map[kind]uint8{kindInsert: 1, kindRemove: 2, kindClaim: 3}[k]
	if last {
		b += 0x80
	}
	return b
}

// checkedBytes is upstream's StoredRecord::checked_bytes: the record's
// bytes after its CRC, which the CRC covers.
func checkedBytes(r storedRecord) []byte {
	var b []byte
	b = binary.LittleEndian.AppendUint64(b, r.lsn)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(r.key)))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(r.value)))
	b = append(b, specKindByte(r.kind, r.lastInAppend))
	b = append(b, r.key...)
	return append(b, r.value...)
}

// specBytes is upstream's StoredRecord::bytes: the CRC32C of the checked
// bytes, then them.
func specBytes(r storedRecord) []byte {
	checked := checkedBytes(r)
	return append(binary.LittleEndian.AppendUint32(nil, crc32.Checksum(checked, castagnoli)), checked...)
}

// readFrom is the one record whose bytes can start b: the fields b's header
// gives, read where a record keeps them, if they are a record's. A record's
// bytes start b only if it is this one (upstream's lemma_starts_with: its
// fields are read from b), and then exactly if its CRC is b's.
func readFrom(b []byte) (storedRecord, bool) {
	if len(b) < 21 {
		return storedRecord{}, false
	}
	keyLen := uint64(binary.LittleEndian.Uint32(b[12:]))
	valueLen := uint64(binary.LittleEndian.Uint32(b[16:]))
	if 21+keyLen+valueLen > uint64(len(b)) {
		return storedRecord{}, false
	}
	for _, k := range specKinds {
		for _, last := range []bool{false, true} {
			if specKindByte(k, last) == b[20] {
				return storedRecord{
					lsn:          binary.LittleEndian.Uint64(b[4:]),
					kind:         k,
					lastInAppend: last,
					key:          b[21 : 21+keyLen],
					value:        b[21+keyLen : 21+keyLen+valueLen],
				}, true
			}
		}
	}
	return storedRecord{}, false
}

func sameRecord(a, b storedRecord) bool {
	return a.lsn == b.lsn && a.kind == b.kind && a.lastInAppend == b.lastInAppend &&
		bytes.Equal(a.key, b.key) && bytes.Equal(a.value, b.value)
}

// checkDecode checks decodeRecord's spec on b: it decodes a record exactly
// when one's bytes start b, and that record, its length theirs.
func checkDecode(t testing.TB, b []byte) {
	t.Helper()
	decoded, ok := decodeRecord(b)
	held, read := readFrom(b)
	starts := read && bytes.HasPrefix(b, specBytes(held))
	if ok != starts {
		t.Fatalf("decoded %v, but a record's bytes start them: %v\n%x", ok, starts, b)
	}
	if ok && (!sameRecord(decoded, held) || decoded.encodedLen() != len(specBytes(held))) {
		t.Fatalf("decoded %+v, want %+v\n%x", decoded, held, b)
	}
}

// checkRoundTrip checks that r encodes as its bytes after any prefix, and
// decodes from them whatever follows.
func checkRoundTrip(t testing.TB, r storedRecord, prefix, suffix []byte) {
	t.Helper()
	encoded := r.encode(bytes.Clone(prefix))
	want := specBytes(r)
	if !bytes.Equal(encoded[:len(prefix)], prefix) || !bytes.Equal(encoded[len(prefix):], want) {
		t.Fatalf("encoded %x, want %x then %x", encoded, prefix, want)
	}
	if r.encodedLen() != len(want) {
		t.Fatalf("length %d, want %d", r.encodedLen(), len(want))
	}
	b := append(encoded[len(prefix):], suffix...)
	decoded, ok := decodeRecord(b)
	if !ok || !sameRecord(decoded, r) {
		t.Fatalf("decoded %+v, %v from %x, want %+v", decoded, ok, b, r)
	}
	checkDecode(t, b)
}

func randomBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return b
}

// randomLength is mostly short, at times empty or long.
func randomLength(rng *rand.Rand) int {
	switch rng.IntN(8) {
	case 0:
		return 0
	case 1:
		return rng.IntN(600)
	}
	return rng.IntN(24)
}

func randomRecord(rng *rand.Rand) storedRecord {
	lsn := rng.Uint64()
	if rng.IntN(4) == 0 {
		lsn = []uint64{0, 1, math.MaxUint64}[rng.IntN(3)]
	}
	return storedRecord{
		lsn:          lsn,
		kind:         specKinds[rng.IntN(len(specKinds))],
		lastInAppend: rng.IntN(2) == 0,
		key:          randomBytes(rng, randomLength(rng)),
		value:        randomBytes(rng, randomLength(rng)),
	}
}

// mutate changes b at a place m picks: a flipped bit, a truncation, a
// byte inserted or dropped, or a slice of b repeated.
func mutate(b []byte, m uint64) []byte {
	if len(b) == 0 {
		return []byte{byte(m)}
	}
	pos := int(m >> 8 % uint64(len(b)))
	b = bytes.Clone(b)
	switch m % 5 {
	case 0:
		b[pos] ^= 1 << (m >> 3 % 8)
	case 1:
		b = b[:pos]
	case 2:
		b = append(b[:pos], append([]byte{byte(m >> 40)}, b[pos:]...)...)
	case 3:
		b = append(b[:pos], b[pos+1:]...)
	case 4:
		end := pos + int(m>>40%uint64(len(b)-pos)) + 1
		b = append(b[:end], append(bytes.Clone(b[pos:end]), b[end:]...)...)
	}
	return b
}

// The kind bytes are a bijection: of_byte gives (k, last) for a byte
// exactly when that is k's byte with last, as upstream's Kind::of_byte
// ensures.
func TestKindByteIsABijection(t *testing.T) {
	for b := range 256 {
		k, last, ok := kindOfByte(uint8(b))
		matches := 0
		for _, kk := range specKinds {
			for _, ll := range []bool{false, true} {
				if specKindByte(kk, ll) == uint8(b) {
					matches++
					if !ok || k != kk || last != ll {
						t.Fatalf("byte %#x: of_byte %v %v %v, the byte of kind %v last %v", b, k, last, ok, kk, ll)
					}
				}
			}
		}
		if ok && matches == 0 {
			t.Fatalf("byte %#x: of_byte %v %v, no kind's byte", b, k, last)
		}
	}
	for _, k := range specKinds {
		for _, last := range []bool{false, true} {
			if k.kindByte(last) != specKindByte(k, last) {
				t.Fatalf("kind %v last %v: byte %#x, want %#x", k, last, k.kindByte(last), specKindByte(k, last))
			}
		}
	}
}

// The CRC is CRC32C, upstream's crc32c crate's: its check value.
func TestCRC32C(t *testing.T) {
	if crc := crc32.Checksum([]byte("123456789"), castagnoli); crc != 0xe3069283 {
		t.Fatalf("%#x", crc)
	}
}

// A record's bytes, as encode writes them, are upstream's: a known record.
func TestRecordBytes(t *testing.T) {
	r := storedRecord{lsn: 0x0102030405060708, kind: kindInsert, lastInAppend: true, key: []byte("k"), value: []byte("vv")}
	checked := []byte{8, 7, 6, 5, 4, 3, 2, 1, 1, 0, 0, 0, 2, 0, 0, 0, 0x81, 'k', 'v', 'v'}
	want := append(binary.LittleEndian.AppendUint32(nil, crc32.Checksum(checked, castagnoli)), checked...)
	if encoded := r.encode(nil); !bytes.Equal(encoded, want) {
		t.Fatalf("%x, want %x", encoded, want)
	}
}

func TestEncodeRoundTrips(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 5000 {
		r := randomRecord(rng)
		checkRoundTrip(t, r, randomBytes(rng, rng.IntN(30)), randomBytes(rng, randomLength(rng)))
	}
}

// Decoding reads a record exactly when one's bytes start the input, and
// that one: on records' bytes changed, and on bytes of no record.
func TestDecodeIsUnique(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for range 20000 {
		r := randomRecord(rng)
		b := append(r.encode(nil), randomBytes(rng, randomLength(rng))...)
		checkDecode(t, mutate(b, rng.Uint64()))
		checkDecode(t, randomBytes(rng, rng.IntN(64)))
	}
}

// Upstream's any_flipped_bit_fails (wal/record.rs).
func TestAnyFlippedBitFails(t *testing.T) {
	r := storedRecord{lsn: math.MaxUint64, kind: kindRemove, key: []byte("key")}
	b := r.encode(nil)
	for bit := range len(b) * 8 {
		flipped := bytes.Clone(b)
		flipped[bit/8] ^= 1 << (bit % 8)
		if _, ok := decodeRecord(flipped); ok {
			t.Fatalf("bit %d", bit)
		}
	}
}

func FuzzRecord(f *testing.F) {
	f.Add(uint64(0), uint8(0x81), []byte("key"), []byte("value"), []byte{}, uint64(0))
	f.Add(uint64(math.MaxUint64), uint8(2), []byte("key"), []byte{}, []byte("tail"), uint64(1<<8|1))
	f.Add(uint64(7), uint8(3), []byte{}, []byte{}, []byte("\x00\x00\x00\x00"), uint64(12<<8|4))
	f.Fuzz(func(t *testing.T, lsn uint64, kindByte uint8, key, value, suffix []byte, m uint64) {
		r := storedRecord{
			lsn:          lsn,
			kind:         specKinds[int(kindByte&0x7f)%len(specKinds)],
			lastInAppend: kindByte&0x80 != 0,
			key:          key,
			value:        value,
		}
		checkRoundTrip(t, r, suffix, suffix)
		b := append(r.encode(nil), suffix...)
		checkDecode(t, mutate(b, m))
		checkDecode(t, suffix)
	})
}
