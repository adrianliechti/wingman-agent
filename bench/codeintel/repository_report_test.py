import copy
import hashlib
import json
from pathlib import Path
import unittest

from repository_report import extract_answer, score_facts, summarize


class RepositoryReportTests(unittest.TestCase):
    def test_prose_is_not_a_factual_failure(self):
        self.assertEqual(score_facts('The result is:\n```json\n{"value":63}\n```', {"value":63}), (True,1))

    def test_multiple_answers_are_not_cherry_picked(self):
        self.assertIsNone(extract_answer('{"value":12} or {"value":63}'))
        self.assertIsNone(extract_answer('{"value":12,"value":63}'))
        self.assertIsNone(extract_answer('{"value":12,"value":63,"nested":{"value":63}}'))

    def test_wrong_values_missing_extra_and_boolean_types(self):
        self.assertEqual(score_facts('{"value":12}', {"value":63}), (False,0))
        self.assertEqual(score_facts('{}', {"value":63}), (False,0))
        self.assertEqual(score_facts('{"value":63,"extra":1}', {"value":63}), (True,1))
        self.assertEqual(score_facts('{"value":1}', {"value":True}), (False,0))

    def sample(self):
        data = Path(__file__).with_name("repo_tasks.json").read_bytes()
        turns = [{"task":t["name"],"final":"","correct":False,"failure":"setup: unavailable","input_tokens":0,"output_tokens":0,"tool_calls":0,"tool_errors":0,"elapsed_ms":0} for t in json.loads(data)["gin"]]
        return {"task_sha256":hashlib.sha256(data).hexdigest(),"prompt_sha256":{"test":"test"},"repositories":{"gin":{}},"configurations":["external_graph"],"repetitions":1,"sessions":[{"model":"test","repository":"gin","configuration":"external_graph","repeat":1,"failure":"unavailable","turns":turns}]}

    def test_setup_failure_stays_in_denominator(self):
        report = self.sample()
        report["sessions"][0]["turns"][0]["trace"] = None
        row = summarize(report)["aggregates"][0]
        self.assertEqual((row["correct"],row["answers"],row["sessions"],row["setup_failed"]),(0,3,1,1))

    def test_duplicate_or_missing_turn_rejected(self):
        report = self.sample()
        report["sessions"].append(copy.deepcopy(report["sessions"][0]))
        with self.assertRaisesRegex(ValueError,"duplicate"):
            summarize(report)
        report = self.sample()
        report["sessions"][0]["turns"].pop()
        with self.assertRaisesRegex(ValueError,"missing"):
            summarize(report)

    def test_incomplete_matrix_is_explicit(self):
        report = self.sample()
        self.assertTrue(summarize(report)["coverage"]["complete"])
        report["repetitions"] = 2
        coverage = summarize(report)["coverage"]
        self.assertFalse(coverage["complete"])
        self.assertEqual(coverage["missing_sessions"], [["test","gin","external_graph",2]])

    def test_unexpected_cell_rejected(self):
        report = self.sample()
        report["sessions"][0]["repeat"] = 2
        with self.assertRaisesRegex(ValueError,"unexpected"):
            summarize(report)


if __name__ == "__main__":
    unittest.main()
