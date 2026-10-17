import copy
import unittest

from compare import compare
from report import CONFIGS


class ComparisonTests(unittest.TestCase):
    def setUp(self):
        self.report = {"model": "fixture", "effort": "low", "repetitions": 3,
                       "fixture_sha256": {"task": "same-source"}, "trials": []}
        for config in CONFIGS:
            for repeat in range(1, 4):
                self.report["trials"].append({
                    "task": "task", "configuration": config, "repeat": repeat,
                    "correct": True, "tool_calls": 2, "input_tokens": 100,
                    "output_tokens": 10, "elapsed_ms": 1000, "tool_errors": 0,
                    "tools": {}, "trace": [{"output_bytes": 50}],
                })

    def test_failed_attempt_stays_in_denominator(self):
        after = copy.deepcopy(self.report)
        after["trials"][0]["correct"] = False
        after["trials"][0]["tool_calls"] = 5
        table = compare(self.report, after)
        self.assertIn("3/3 → 2/3 | 2.0 → 3.0", table)

    def test_incomparable_or_selected_trials_are_rejected(self):
        for mutation in [
            lambda r: r["trials"].pop(),
            lambda r: r["trials"].append(r["trials"][0]),
            lambda r: r.update(model="other"),
            lambda r: r.update(effort="high"),
            lambda r: r["fixture_sha256"].update(task="changed-source"),
            lambda r: r["fixture_sha256"].clear(),
        ]:
            with self.subTest(mutation=mutation):
                after = copy.deepcopy(self.report)
                mutation(after)
                with self.assertRaises(ValueError):
                    compare(self.report, after)


if __name__ == "__main__":
    unittest.main()
