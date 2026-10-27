import unittest

from cpu_compare import MASK64, key_of, mix, replacement_checksum

PATTERNS = ("mixed", "sequential", "clustered")


def simulate_replacements(size, query_count, rounds, skip_writes=False, pattern="mixed"):
    queries = [key_of(mix(i + 17) % size, size, pattern) for i in range(query_count)]
    values = {key: key for key in queries}
    full_sum = masked_sum = 0
    for round_index in range(rounds):
        for key in queries:
            old = values[key]
            full_sum += old
            masked_sum += old & ~1
            if not skip_writes:
                values[key] = key ^ (round_index & 1)
    final_ok = all(values[key] == key ^ ((rounds - 1) & 1) for key in queries)
    return full_sum & MASK64, masked_sum & MASK64, final_ok


class ReplacementChecksumTests(unittest.TestCase):
    def test_formula_matches_stateful_operations(self):
        for pattern in PATTERNS:
            for size in (1, 2, 3, 7, 8, 17, 32, 257):
                for query_count in (8, 16, 32, 64):
                    for rounds in range(1, 8):
                        with self.subTest(pattern=pattern, size=size, queries=query_count, rounds=rounds):
                            expected, _, final_ok = simulate_replacements(size, query_count, rounds, pattern=pattern)
                            self.assertEqual(replacement_checksum(size, query_count, rounds, pattern), expected)
                            self.assertTrue(final_ok)

    def test_duplicates_use_new_value_after_first_occurrence(self):
        # The sole key is zero. In round one only the first old value is zero;
        # the other seven replacements observe the value written earlier.
        self.assertEqual(replacement_checksum(1, 8, 1, "mixed"), 0)
        self.assertEqual(replacement_checksum(1, 8, 2, "mixed"), 7)
        self.assertEqual(replacement_checksum(1, 8, 3, "mixed"), 8)
        self.assertEqual(replacement_checksum(1, 8, 4, "mixed"), 15)

    def test_old_mask_cannot_detect_skipped_writes(self):
        for rounds in (2, 3, 4):
            with self.subTest(rounds=rounds):
                full, masked, _ = simulate_replacements(1, 8, rounds)
                skipped_full, skipped_masked, final_ok = simulate_replacements(1, 8, rounds, True)
                self.assertEqual(masked, skipped_masked)
                self.assertNotEqual(full, skipped_full)
                self.assertEqual(final_ok, rounds % 2 == 1)


if __name__ == "__main__":
    unittest.main()
