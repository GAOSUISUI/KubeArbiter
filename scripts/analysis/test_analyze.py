import csv
import io
import json
import sys
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import analyze  # noqa: E402


def write_jsonl(tmp: Path, records: list[dict]) -> Path:
    path = tmp / "samples.jsonl"
    path.write_text(
        "\n".join(json.dumps(r, ensure_ascii=False) for r in records) + "\n",
        encoding="utf-8",
    )
    return path


class PercentileTest(unittest.TestCase):
    def test_linear_interpolation(self):
        values = [1.0, 2.0, 3.0, 4.0]
        self.assertAlmostEqual(analyze.percentile(values, 0.50), 2.5)
        self.assertAlmostEqual(analyze.percentile(values, 0.95), 3.85)
        self.assertAlmostEqual(analyze.percentile(values, 0.0), 1.0)
        self.assertAlmostEqual(analyze.percentile(values, 1.0), 4.0)

    def test_single_value(self):
        self.assertEqual(analyze.percentile([7.0], 0.95), 7.0)

    def test_empty_rejected(self):
        with self.assertRaises(ValueError):
            analyze.percentile([], 0.5)


class BootstrapTest(unittest.TestCase):
    def test_deterministic_with_seed(self):
        values = [10.0, 12.0, 9.0, 11.0, 30.0]
        first = analyze.bootstrap_ci(values, 500, seed=7)
        second = analyze.bootstrap_ci(values, 500, seed=7)
        self.assertEqual(first, second)
        self.assertLessEqual(first[0], first[1])

    def test_interval_contains_mean(self):
        values = [1.0, 2.0, 3.0, 4.0, 5.0]
        low, high = analyze.bootstrap_ci(values, 1000, seed=42)
        mean = sum(values) / len(values)
        self.assertLessEqual(low, mean)
        self.assertGreaterEqual(high, mean)

    def test_single_value_interval(self):
        self.assertEqual(analyze.bootstrap_ci([3.0], 100, seed=1), (3.0, 3.0))

    def test_rejects_zero_iterations(self):
        with self.assertRaises(ValueError):
            analyze.bootstrap_ci([1.0, 2.0], 0, seed=1)


class LoadSamplesTest(unittest.TestCase):
    def test_groups_and_sorts_labels(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            path = write_jsonl(tmp_path, [
                {"experiment": "E1", "metric": "latency_ms", "value": 10,
                 "labels": {"policy": "BIN_PACK"}, "unit": "ms"},
                {"experiment": "E1", "metric": "latency_ms", "value": 20,
                 "labels": {"policy": "BIN_PACK"}, "unit": "ms"},
                {"experiment": "E1", "metric": "latency_ms", "value": 5,
                 "labels": {"policy": "BALANCED"}, "unit": "ms"},
            ])
            samples = analyze.load_samples(path, allow_synthetic=False)
        self.assertEqual(len(samples), 3)
        groups = analyze.group_samples(samples)
        self.assertEqual(len(groups), 2)
        rows = analyze.build_rows(groups, iterations=200, seed=42)
        balanced = [r for r in rows if "BALANCED" in r["labels"]]
        self.assertEqual(len(balanced), 1)
        self.assertEqual(balanced[0]["n"], 1)
        self.assertEqual(balanced[0]["mean"], 5.0)

    def test_synthetic_rejected_by_default(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = write_jsonl(Path(tmp), [
                {"experiment": "E1", "metric": "m", "value": 1, "synthetic": True},
            ])
            with self.assertRaises(analyze.AnalysisError) as ctx:
                analyze.load_samples(path, allow_synthetic=False)
        self.assertIn("synthetic", str(ctx.exception))

    def test_synthetic_allowed_when_asked(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = write_jsonl(Path(tmp), [
                {"experiment": "E1", "metric": "m", "value": 1, "synthetic": True},
            ])
            samples = analyze.load_samples(path, allow_synthetic=True)
        self.assertEqual(len(samples), 1)

    def test_malformed_json_reports_line(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "bad.jsonl"
            path.write_text(
                '{"experiment": "E1", "metric": "m", "value": 1}\nnot-json\n',
                encoding="utf-8",
            )
            with self.assertRaises(analyze.AnalysisError) as ctx:
                analyze.load_samples(path, allow_synthetic=False)
        self.assertIn("第 2 行", str(ctx.exception))

    def test_rejects_bool_and_non_finite_value(self):
        records = [
            {"experiment": "E1", "metric": "m", "value": True},
            {"experiment": "E1", "metric": "m", "value": float("inf")},
        ]
        for record in records:
            with tempfile.TemporaryDirectory() as tmp:
                path = write_jsonl(Path(tmp), [record])
                with self.assertRaises(analyze.AnalysisError):
                    analyze.load_samples(path, allow_synthetic=False)

    def test_rejects_missing_experiment(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = write_jsonl(Path(tmp), [{"experiment": "", "metric": "m", "value": 1}])
            with self.assertRaises(analyze.AnalysisError) as ctx:
                analyze.load_samples(path, allow_synthetic=False)
        self.assertIn("experiment", str(ctx.exception))

    def test_empty_input_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "empty.jsonl"
            path.write_text("\n\n", encoding="utf-8")
            with self.assertRaises(analyze.AnalysisError):
                analyze.load_samples(path, allow_synthetic=False)


class OutputTest(unittest.TestCase):
    def test_markdown_and_csv(self):
        rows = [{
            "experiment": "E2", "metric": "jain", "unit": "", "labels": '{"allocator": "DRF"}',
            "n": 3, "mean": 0.9, "p50": 0.9, "p95": 0.99, "p99": 0.999,
            "min": 0.8, "max": 1.0, "ci95_low": 0.85, "ci95_high": 0.95,
        }]
        md = analyze.format_markdown(rows)
        self.assertIn("| experiment | metric |", md)
        self.assertIn("0.9000", md)

        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "summary.csv"
            analyze.write_csv(rows, out)
            with out.open(encoding="utf-8") as fh:
                parsed = list(csv.DictReader(fh))
        self.assertEqual(len(parsed), 1)
        self.assertEqual(parsed[0]["experiment"], "E2")
        self.assertEqual(parsed[0]["labels"], '{"allocator": "DRF"}')


class CliTest(unittest.TestCase):
    def test_main_success(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = write_jsonl(Path(tmp), [
                {"experiment": "E1", "metric": "m", "value": 1},
                {"experiment": "E1", "metric": "m", "value": 3},
            ])
            with redirect_stdout(io.StringIO()):
                code = analyze.main(["--input", str(path), "--bootstrap", "100"])
        self.assertEqual(code, 0)

    def test_main_returns_2_on_bad_input(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = write_jsonl(Path(tmp), [
                {"experiment": "E1", "metric": "m", "value": 1, "synthetic": True},
            ])
            with redirect_stdout(io.StringIO()):
                code = analyze.main(["--input", str(path)])
        self.assertEqual(code, 2)


if __name__ == "__main__":
    unittest.main()
