package gobbtree

import (
	"fmt"
	"math"
	"sync"
	"testing"
)

func TestRetiredImageStaysInvalid(t *testing.T) {
	m := New[float64, uint64]()
	for k := float64(0); k < 40; k += 2 {
		m.Insert(k, uint64(k)*10)
	}
	beforeInsert := m.root.Load()
	m.Insert(15, 150)
	beforeDelete := m.root.Load()
	m.Delete(15)
	for _, old := range []*searchImage[float64, uint64, natural[float64]]{beforeInsert, beforeDelete} {
		if _, _, valid := old.readValue(16, numericCode(float64(16)), &m.version, m.version.Load()); valid {
			t.Fatal("retired image accepted a reused value slot")
		}
	}
	if value, ok := m.Get(16); !ok || value != 160 {
		t.Fatal("current image rejected its value")
	}
}

func TestOptimisticReferenceValues(t *testing.T) {
	type value struct {
		Text    string
		Numbers []int
	}
	m := New[int, value]()
	for i := 0; i < 3000; i++ {
		m.Insert(i, value{fmt.Sprint(i), []int{i, i + 1}})
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Go(func() {
			for pass := 0; pass < 4; pass++ {
				for k := worker; k < 5000; k += 4 {
					m.Delete(k)
					m.Insert(k, value{fmt.Sprint(k), []int{k, k + 1}})
				}
			}
		})
	}
	for worker := 0; worker < 4; worker++ {
		wg.Go(func() {
			for k := 0; k < 20000; k++ {
				key := k % 5000
				if v, ok := m.Get(key); ok {
					if v.Text != fmt.Sprint(key) || len(v.Numbers) != 2 || v.Numbers[0] != key || v.Numbers[1] != key+1 {
						t.Error("torn reference value")
					}
				}
			}
		})
	}
	wg.Wait()
}
func testWordValue[V comparable](t *testing.T, value V) {
	t.Helper()
	m := New[uint64, V]()
	m.Insert(0, value)
	if got, ok := m.Get(0); !ok || got != value {
		t.Fatalf("word roundtrip %T: %v", value, got)
	}
	if n := testing.AllocsPerRun(100, func() { m.Insert(0, value); m.Get(0) }); n != 0 {
		t.Fatalf("word %T allocates: %v", value, n)
	}
}
func TestAtomicValueKinds(t *testing.T) {
	testWordValue(t, uint64(0xffffffffffffffff))
	testWordValue(t, int(-7))
	testWordValue(t, int64(-8))
	testWordValue(t, uint(9))
	testWordValue(t, uint32(0xffffffff))
	testWordValue(t, int32(-9))
	testWordValue(t, uint16(65535))
	testWordValue(t, int16(-32768))
	testWordValue(t, uint8(255))
	testWordValue(t, int8(-128))
	testWordValue(t, float64(-2.5))
	testWordValue(t, float32(3.5))
	testWordValue(t, true)
	testWordValue(t, uintptr(17))
}

// TestConcurrentEmptyAndRebuild empties and refills a map under readers.
// Clear is the owner's, as upstream's, so the writer removes every key.
func TestConcurrentEmptyAndRebuild(t *testing.T) {
	m := New[uint64, uint64]()
	var wg sync.WaitGroup
	for i := uint64(0); i < 3000; i++ {
		m.Insert(i, i)
	}
	wg.Go(func() {
		for r := 0; r < 20; r++ {
			for i := uint64(0); i < 3000; i++ {
				m.Delete(i)
			}
			for i := uint64(0); i < 3000; i++ {
				m.Insert(i, i)
			}
		}
	})
	for w := 0; w < 4; w++ {
		wg.Go(func() {
			for i := uint64(0); i < 60000; i++ {
				if value, ok := m.Get(i % 3000); ok && value != i%3000 {
					t.Error("wrong value after clear")
				}
				if i%64 == 0 {
					var keys [8]uint64
					var dst [8]Result[uint64]
					for j := range keys {
						keys[j] = (i + uint64(j)*191) % 3000
					}
					m.GetMany(keys[:], dst[:])
					for j, v := range dst {
						if v.OK && v.Value != keys[j] {
							t.Error("wrong batch value after clear")
						}
					}
				}
			}
		})
	}
	wg.Wait()
	if seqLen(m.All()) != 3000 {
		t.Fatal(seqLen(m.All()))
	}
}

func TestAtomicFloatBits(t *testing.T) {
	m := New[int, float64]()
	for _, bits := range []uint64{0, 1 << 63, 0x7ff8000000000123, 0xfff0000000000000} {
		m.Insert(0, math.Float64frombits(bits))
		v, ok := m.Get(0)
		if !ok || math.Float64bits(v) != bits {
			t.Fatalf("float64 bits: %x != %x", math.Float64bits(v), bits)
		}
	}
	f := New[int, float32]()
	for _, bits := range []uint32{0, 1 << 31, 0x7fc00123, 0xff800000} {
		f.Insert(0, math.Float32frombits(bits))
		v, ok := f.Get(0)
		if !ok || math.Float32bits(v) != bits {
			t.Fatalf("float32 bits: %x != %x", math.Float32bits(v), bits)
		}
	}
}

func TestOptimisticStringBatch(t *testing.T) {
	m := New[string, uint64]()
	model := make(map[string]uint64)
	keys := make([]string, 3000)
	for i := range keys {
		key := fmt.Sprintf("tenant/prefix/%08d/entry\x00", i)
		keys[i] = key
		model[key] = uint64(i)
		m.Insert(key, uint64(i))
	}
	for i, key := range keys {
		if i%3 == 0 {
			m.Delete(key)
			delete(model, key)
		}
	}
	for start := 0; start < len(keys); start += 13 {
		var queries [17]string
		var dst [17]Result[uint64]
		for j := range queries {
			queries[j] = keys[(start+j*173)%len(keys)]
			if j%5 == 0 {
				queries[j] += "missing"
			}
		}
		m.GetMany(queries[:], dst[:])
		for j, key := range queries {
			want, ok := model[key]
			if dst[j].Value != want || dst[j].OK != ok {
				t.Fatalf("string batch %q: %+v", key, dst[j])
			}
		}
	}
}
