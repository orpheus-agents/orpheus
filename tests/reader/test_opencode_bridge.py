import importlib.util
import json
import pathlib
import tempfile
import unittest

location = (
    pathlib.Path(__file__).resolve().parents[2] / "internal/harness/opencode/bridge.py"
)
spec = importlib.util.spec_from_file_location("opencode_bridge", location)
bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)


class Stub(bridge.Server):
    def __init__(self):
        self.run = "A"
        self.path = "/api/session/ses_1"
        self.experimental = "/api/experimental/session/ses_1"
        self.fenced = False
        self.empty = True
        self.entries = []
        self.queue = {}
        self.messages = []
        self.active = False
        self.wakes = 0
        self.log = []

    def request(self, path, method="GET", body=None):
        self.log.append((path, method))
        if path == "/api/credential":
            if method == "GET":
                return self.entries
            entry = dict(body)
            entry["active"] = entry.pop("activate")
            self.entries.append(entry)
            return entry
        if path.startswith("/api/credential/") and method == "DELETE":
            self.entries.clear()
            return
        if path.endswith("/inbox"):
            return list(self.queue.values())
        if "/inbox/" in path and method == "DELETE":
            del self.queue[path.rsplit("/", 1)[1]]
            return
        if path.endswith(("/context", "/permission")):
            return []
        if path.endswith("/export"):
            return {"messages": self.messages.copy(), "info": {"tokens": {}}}
        if "/message/" in path:
            mid = path.rsplit("/", 1)[1]
            for m in self.messages:
                if m["id"] == mid:
                    return m
            raise bridge.Failure(404)
        if path.endswith("/prompt"):
            mid = body["id"]
            result = self.queue.setdefault(
                mid,
                {
                    "id": mid,
                    "type": "user",
                    "payload": {k: body[k] for k in ("text", "metadata")},
                },
            )
            self.wakes += int(body["resume"])
            return result
        raise AssertionError((method, path))

    def project(self):
        for row in self.queue.values():
            self.messages.append(dict(id=row["id"], type="user", **row["payload"]))
        self.queue.clear()


class BridgeTest(unittest.TestCase):
    def test_lost_ack_partial_batch_and_late_input(self):
        s = Stub()
        p = {
            "operation": "op",
            "inputs": [{"id": "x", "text": "one"}, {"id": "y", "text": "one"}],
            "start": True,
        }
        s.deliver(dict(p, inputs=p["inputs"][:1], start=False))
        s.deliver(dict(p, replay=True))
        s.deliver(dict(p, replay=True))
        self.assertEqual(s.wakes, 1)
        s.project()
        s.deliver(dict(p, replay=True))
        self.assertEqual(s.wakes, 1)
        s.messages.append({"id": "idle", "type": "idle", "outcome": "succeeded"})
        late = dict(p, inputs=[{"id": "z", "text": "late"}], start=False)
        s.deliver(late)
        snapshot = s.snapshot()
        self.assertTrue(snapshot["fenced"])
        self.assertEqual(list(s.queue), [])
        with self.assertRaises(bridge.Failure):
            s.deliver(late)
        self.assertEqual(
            [m["id"] for m in snapshot["messages"]], ["msg_x", "msg_y", "idle"]
        )

    def test_conflicting_200_and_foreign_run(self):
        s = Stub()
        args = {
            "run": "A",
            "operation": "op",
            "inputs": [{"id": "one", "text": "first"}],
            "start": False,
        }
        s.call("deliver", args)
        with self.assertRaises(bridge.Failure):
            s.call("deliver", dict(args, inputs=[{"id": "one", "text": "changed"}]))
        with self.assertRaises(bridge.Failure):
            s.call("deliver", dict(args, run="B"))

    def test_permission_rejection_and_large_projection(self):
        s = Stub()
        original = s.request
        replies = []

        def request(path, method="GET", body=None):
            if path.endswith("/permission"):
                return [{"id": "ask"}]
            if path.endswith("/permission/ask/reply"):
                replies.append(body)
                return None
            return original(path, method, body)

        s.request = request
        with tempfile.TemporaryDirectory() as root:
            s.data = pathlib.Path(root)
            s.messages = [
                {
                    "id": "msg_input",
                    "type": "user",
                    "metadata": {"orpheus_run_id": "A"},
                    "text": "x" * (5 * 1024 * 1024),
                }
            ]
            frame = s.snapshot_frame()
            saved = json.loads((s.data / "projection.json").read_text())
            self.assertEqual(frame["ticket"], saved["ticket"])
            self.assertEqual(saved["snapshot"]["messages"], s.messages)
            self.assertEqual(replies[0]["decision"], "reject")

    def test_credential_replacement_requires_clean_baseline(self):
        s = Stub()
        first = {
            "id": "cred_a",
            "integrationID": "opencode",
            "label": "account",
            "active": True,
            "value": {"type": "oauth", "access": "original"},
        }
        newer = dict(first, value={"type": "oauth", "access": "updated"})
        s.seed(first, "")
        self.assertEqual(s.log, [("/api/credential", "POST")])
        s.seed(first, "")
        with self.assertRaises(bridge.Failure):
            s.seed(newer, "")
        self.assertEqual(s.entries, [first])
        replacement = dict(newer, id="cred_new")
        s.seed(replacement, first["id"])
        self.assertEqual(s.entries, [replacement])
