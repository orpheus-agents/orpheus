"""Compatibility and golden fixtures for the standalone Python 3.10 reader."""

import ast
import importlib.util
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]
SOURCE = (ROOT / "internal/harness/codex/native_reader.py").read_text()
SPEC = importlib.util.spec_from_file_location(
    "native_reader", ROOT / "internal/harness/codex/native_reader.py"
)
READER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(READER)


class ReaderTests(unittest.TestCase):
    def read(self, records, mode="offline", limit=524288, offset=0, tail=b""):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "history.jsonl"
            raw = b"".join((json.dumps(r) + "\n").encode() for r in records)
            path.write_bytes(raw + tail)
            out = subprocess.check_output(
                [
                    sys.executable,
                    "-c",
                    SOURCE + "\n" + mode + "()\n",
                    str(path),
                    str(offset),
                    str(limit),
                ]
            )
            return json.loads(out), len(raw)

    def fixture(self, name):
        return json.loads((ROOT / "tests/fixtures" / name).read_text())

    def test_python_310_syntax(self):
        ast.parse(SOURCE, feature_version=(3, 10))

    def test_complete_lines_only(self):
        records = self.fixture("codex-native-history.json")["records"]
        for mode in ("journal", "offline", "index"):
            data, size = self.read(records, mode=mode, tail=b'{"unfinished"')
            if mode != "index":
                self.assertEqual(data["offset"], size)

    def test_captured_streamed_output(self):
        data, _ = self.read(self.fixture("codex-streamed-output.json")["records"])
        text = "".join(o["result"].get("text", "") for o in data["outputs"].values())
        self.assertIn("tool-start", text)
        self.assertIn("tool-end", text)
        self.assertTrue(
            any(o["completeness"] == "complete" for o in data["outputs"].values())
        )

    def test_captured_file_change(self):
        data, _ = self.read(self.fixture("codex-file-change.json")["records"])
        output = next(iter(data["outputs"].values()))
        self.assertEqual(output["completeness"], "complete")
        self.assertIn("Success", output["result"]["value"]["stdout"])

    def test_desktop_ids_and_unrepresented_control_outputs(self):
        records = self.fixture("codex-streamed-output.json")["records"]
        # Desktop execution IDs differ from model call IDs. Explicit process
        # binding must still recover output, while unrelated controls add no items.
        for record in records:
            payload = record.get("payload", {})
            if record.get("type") == "event_msg":
                item = payload.get("item", {})
                if item.get("type") == "CommandExecution":
                    item["id"] = "exec_desktop"
                    item["process_id"] = 42
            elif payload.get("type") == "function_call":
                payload["call_id"] = "call_model"
                payload["name"] = "functions.write_stdin"
                payload["arguments"] = '{"session_id":42}'
            elif payload.get("type") == "function_call_output":
                payload["call_id"] = "call_model"
        for name in ("update_plan", "view_image", "wait"):
            records += [
                {
                    "type": "response_item",
                    "payload": {
                        "type": "function_call",
                        "name": name,
                        "call_id": name,
                        "arguments": "{}",
                    },
                },
                {
                    "type": "response_item",
                    "payload": {
                        "type": "function_call_output",
                        "call_id": name,
                        "output": "control-only",
                    },
                },
            ]
        data, size = self.read(records)
        self.assertEqual(data["offset"], size)
        self.assertIn("exec_desktop", data["outputs"])
        self.assertIn("tool-end", data["outputs"]["exec_desktop"]["result"]["text"])
        self.assertNotIn("control-only", json.dumps(data["outputs"]))

    def test_utf8_combined_budget(self):
        for value in ("я" * 1000, {"stdout": "x" * 1000, "stderr": "y" * 1000}):
            result, completeness, reason = READER.bounded(value, 101)
            self.assertEqual((completeness, reason), ("truncated", "orpheus_limit"))
            self.assertLessEqual(
                len(result["head"].encode()) + len(result["tail"].encode()), 101
            )
            self.assertGreater(result["original_bytes"], 101)

    def test_incomplete_source_has_no_size(self):
        result, completeness, reason = READER.bounded("Warning: truncated output", 100)
        self.assertIsNone(result["original_bytes"])
        self.assertEqual((completeness, reason), ("truncated", "harness_limit"))

    def test_restart_index_does_not_replay_outputs(self):
        records = self.fixture("codex-native-history.json")["records"]
        _, size = self.read(records)
        index, _ = self.read(records, mode="index", offset=size)
        self.assertTrue(index["completed"])
        self.assertTrue(index["orders"])
        self.assertTrue(index["commands"])
        self.assertNotIn("outputs", index)
        tail, _ = self.read(records, mode="journal", offset=size)
        self.assertEqual(tail["items"], [])


if __name__ == "__main__":
    unittest.main()
