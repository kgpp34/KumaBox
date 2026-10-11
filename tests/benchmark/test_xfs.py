"""Check measurement aggregation without requiring Linux, root, or XFS."""

import unittest
from pathlib import Path
from unittest.mock import patch

import xfs


class MeasurementTests(unittest.TestCase):
    def test_parallel_requests_and_batch_times_stay_separate(self):
        rows = [{"scenario": "parallel", "iteration": index, "total_ms": index + 1,
                 "individual_ms": [1, 2, 3, 4]} for index in range(5)]
        result, = xfs.summarize_records(rows)
        self.assertEqual(result["n"], 5)
        self.assertIsNone(result["total_ms"]["p95"])
        self.assertEqual(result["individual_ms"]["n"], 20)
        self.assertEqual(result["individual_ms"]["p50"], 2.5)
        self.assertEqual(result["individual_ms"]["p95"], 4)

    def test_twenty_samples_enable_interpolated_p95(self):
        rows = [{"scenario": "write", "iteration": index, "total_ms": index}
                for index in range(20)]
        rows.append({"scenario": "source_layout", "logical_bytes": 1000})
        result, = xfs.summarize_records(rows)
        self.assertAlmostEqual(result["total_ms"]["p95"], 18.05)

    def test_physical_union_counts_shared_and_overlapping_blocks_once(self):
        mapped = {"golden": [{"physical": 100, "length": 100}],
                  "clone": [{"physical": 100, "length": 100},
                            {"physical": 180, "length": 40},
                            {"physical": 300, "length": 10}]}
        with patch.object(xfs, "extents", side_effect=lambda path: mapped[path.name]):
            self.assertEqual(xfs.physical_union([Path("golden"), Path("clone")]), 130)


if __name__ == "__main__":
    unittest.main()
