import unittest

from cpu_compare import (MASK64, check_scale_agreement, key_of, median, mix, parse_scale_line, replacement_checksum,
                         scale_backends, scale_cells, scale_command, summarize_scaling)

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


class ScalingTests(unittest.TestCase):
    def test_parse_scale_line(self):
        row = parse_scale_line("scale_r90_w12 3.250 ns/op checksum=18446744073709551615 state=7 count=262144")
        self.assertEqual(row, {"mix": "r90", "workers": 12, "ns_per_op": 3.25, "checksum": MASK64, "state": 7, "count": 262144})
        for bad in ("map_parallel4_hit 1.0 ns/op checksum=1", "scale_read_w4 1.0 ns/op checksum=1", "scale_read_w4 1.0 ns/op checksum=1 state=2 count=x"):
            with self.subTest(line=bad), self.assertRaises(RuntimeError):
                parse_scale_line(bad)

    def test_cells_restrict_fifty_million_to_reads(self):
        cells = scale_cells([262144], ["mixed"], ["read", "churn"], [1, 4], fifty=True)
        self.assertEqual(cells, [(262144, "mixed", "read", 1), (262144, "mixed", "read", 4), (262144, "mixed", "churn", 1), (262144, "mixed", "churn", 4), (50_000_000, "mixed", "read", 1), (50_000_000, "mixed", "read", 4)])
        self.assertNotIn(50_000_000, {c[0] for c in scale_cells([262144], ["mixed"], ["churn"], [1], fifty=True)})

    def test_backend_rotation_alternates(self):
        backends = [("go", "g"), ("rust", "r")]
        firsts = [scale_backends(backends, repeat, index)[0][0] for repeat in range(2) for index in range(3)]
        self.assertEqual(firsts, ["go", "rust", "go", "rust", "go", "rust"])

    def test_commands_select_one_cell(self):
        go = scale_command("go", "compare-go", 5000000, "clustered", "scan", 8, 4096, 1024)
        self.assertEqual(go, ["compare-go", "-kind", "map", "-size", "5000000", "-threads", "8", "-queries", "1024", "-scale", "scan", "-ops", "4096", "-keys=clustered"])
        rust = scale_command("rust", "compare", 5000000, "clustered", "scan", 8, 4096, 1024)
        self.assertEqual(rust, ["compare", "5000000", "1", "8", "1024", "map", "scale=scan", "ops=4096", "keys=clustered"])

    def test_agreement_is_per_cell(self):
        expected = {}
        row = {"checksum": 1, "state": 2, "count": 3}
        check_scale_agreement(expected, ("a", 1), row, "go")
        check_scale_agreement(expected, ("a", 1), dict(row), "rust")
        check_scale_agreement(expected, ("a", 2), {"checksum": 9, "state": 2, "count": 3}, "go")
        for field in row:
            with self.subTest(field=field), self.assertRaises(RuntimeError):
                check_scale_agreement(expected, ("a", 1), {**row, field: row[field] + 1}, "rust")

    def test_median(self):
        self.assertEqual(median([3, 1, 2]), 2)
        self.assertEqual(median([4, 1, 2, 3]), 2.5)

    def test_summary_medians_speedup_and_ratio(self):
        def run(language, workers, ns):
            return {"language": language, "size": 8, "keys": "mixed", "mix": "read", "workers": workers, "ns_per_op": ns}
        runs = [run("go", 1, 10), run("go", 1, 12), run("go", 1, 100),
                run("go", 4, 4), run("rust", 1, 20), run("rust", 4, 5), run("rust", 4, 5), run("rust", 4, 50)]
        rows = summarize_scaling(runs)
        self.assertEqual(rows, [
            {"size": 8, "keys": "mixed", "mix": "read", "workers": 1, "go_mops": 83.333, "go_speedup": 1.0, "rust_mops": 50.0, "rust_speedup": 1.0, "go_over_rust": 1.667},
            {"size": 8, "keys": "mixed", "mix": "read", "workers": 4, "go_mops": 250.0, "go_speedup": 3.0, "rust_mops": 200.0, "rust_speedup": 4.0, "go_over_rust": 1.25},
        ])

    def test_summary_orders_mixes_and_tolerates_missing_baseline(self):
        runs = [{"language": "go", "size": 8, "keys": "mixed", "mix": mix_name, "workers": 2, "ns_per_op": 1} for mix_name in ("scan", "read")]
        rows = summarize_scaling(runs)
        self.assertEqual([r["mix"] for r in rows], ["read", "scan"])
        self.assertIsNone(rows[0]["go_speedup"])
        self.assertNotIn("go_over_rust", rows[0])


if __name__ == "__main__":
    unittest.main()
