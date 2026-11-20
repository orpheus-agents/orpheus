"""Private, run-scoped stdio transport for OpenCode V2's loopback API."""

import base64
import fcntl
import json
import os
import pathlib
import re
import secrets
import signal
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from urllib.parse import quote


class Failure(Exception):
    def __init__(self, code):
        self.code = code


class LoopbackOnly(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Server:
    def __init__(self, session, run):
        self.session = session
        self.run = run
        self.path = "/api/session/" + session
        self.experimental = "/api/experimental/session/" + session
        self.fenced = False
        self.write_lock = threading.Lock()
        self.http = urllib.request.build_opener(
            urllib.request.ProxyHandler({}), LoopbackOnly()
        )
        data = pathlib.Path.home() / ".local/share/orpheus-opencode" / session
        data.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.data = data
        self.lock = (data / "owner.lock").open("a")
        # The child also holds the lock. SIGKILL of its supervisor closes its
        # stdin; a replacement cannot open the same database before it exits.
        fcntl.flock(self.lock, fcntl.LOCK_EX)
        self.empty = not (data / "opencode").exists()
        password = secrets.token_urlsafe(32)
        self.headers = {
            "Authorization": "Basic "
            + base64.b64encode(("opencode:" + password).encode()).decode(),
            "Content-Type": "application/json",
        }
        self.child = subprocess.Popen(
            ["opencode", "serve", "--stdio", "--hostname", "127.0.0.1", "--port", "0"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            env=dict(
                os.environ, XDG_DATA_HOME=str(data), OPENCODE_SERVER_PASSWORD=password
            ),
            pass_fds=(self.lock.fileno(),),
        )
        line = self.child.stdout.readline()
        endpoint = re.search(rb"http://127\.0\.0\.1:\d+", line)
        if not endpoint:
            self.close()
            raise Failure("startup")
        self.base = endpoint[0].decode()
        deadline = time.monotonic() + 12
        while time.monotonic() < deadline:
            try:
                self.request("/api/session/active", timeout=2)
                break
            except (Failure, OSError):
                if self.child.poll() is not None:
                    raise Failure("startup") from None
                time.sleep(0.05)
        else:
            raise Failure("startup")

    def close(self):
        self.child.stdin.close()
        try:
            self.child.wait(timeout=4)
        except subprocess.TimeoutExpired:
            self.child.kill()
            self.child.wait()
        self.lock.close()

    def output(self, value):
        with self.write_lock:
            print(json.dumps(value, separators=(",", ":")), flush=True)

    def events(self):
        # Events only invalidate the projection. No event data crosses the wire.
        while self.child.poll() is None:
            try:
                req = urllib.request.Request(
                    self.base + "/api/event", headers=self.headers
                )
                with self.http.open(req, timeout=45) as stream:
                    for line in stream:
                        if line.startswith(b"data:"):
                            self.output({"method": "harness/changed"})
            except (OSError, ValueError):
                time.sleep(0.5)

    def request(self, path, method="GET", body=None, timeout=20):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(
            self.base + path, data, self.headers, method=method
        )
        try:
            with self.http.open(req, timeout=timeout) as stream:
                raw = stream.read()
                if not raw:
                    return None
                response = json.loads(raw)
                return response.get("data", response)
        except urllib.error.HTTPError as err:
            raise Failure(err.code) from None

    def entry(self, provider):
        rows = self.request("/api/credential")
        if not rows:
            return None
        if (
            len(rows) != 1
            or rows[0]["integrationID"] != provider
            or not rows[0]["active"]
        ):
            raise Failure("credential_conflict")
        return rows[0]

    def seed(self, entry, expected_id):
        data = dict(entry)
        data["activate"] = data.pop("active")
        if self.empty:
            self.empty = False
        else:
            current = self.entry(entry["integrationID"])
            if current == entry:
                return
            if current:
                if current["id"] != expected_id:
                    raise Failure("credential_conflict")
                self.request(
                    "/api/credential/" + quote(current["id"], safe=""), "DELETE"
                )
        self.request("/api/credential", "POST", data)

    def snapshot(self, dead=False):
        if not dead:
            for req in self.request(self.path + "/permission"):
                self.request(
                    self.path + "/permission/" + quote(req["id"], safe="") + "/reply",
                    "POST",
                    {
                        "decision": "reject",
                        "message": "No interactive operator is available.",
                    },
                )
        # Read live state first. The subsequent full durable export contains the
        # authoritative completed versions and survives context compaction.
        live = self.request(self.path + "/context")
        export = self.request(self.experimental + "/export")
        rows = export["messages"]
        saved = {m["id"] for m in rows}
        for m in live:
            if m["id"] not in saved and m["type"] in (
                "assistant",
                "compaction",
                "shell",
            ):
                rows.append(m)
        run = None
        groups = {}
        for m in rows:
            kind = m["type"]
            if kind == "user":
                run = m.get("metadata", {}).get("orpheus_run_id")
            if not run:
                continue
            if run not in groups:
                groups[run] = []
            group = groups[run]
            if kind == "user":
                group.append({k: m[k] for k in ("id", "type", "text", "metadata")})
            elif kind == "assistant":
                content = []
                for part in m["content"]:
                    if part["type"] == "text":
                        content.append({"type": "text", "text": part["text"]})
                    elif part["type"] == "reasoning":
                        # Keep the slot for stable native IDs across worker upgrades.
                        content.append({"type": "reasoning"})
                    elif part["type"] == "tool":
                        state = part["state"]
                        item = {
                            k: state[k]
                            for k in ("status", "input", "error")
                            if k in state
                        }
                        if "content" in state:
                            item["content"] = [
                                x["text"]
                                if x.get("type") == "text"
                                else {"type": x.get("type")}
                                for x in state["content"]
                            ]
                        content.append(
                            {
                                "type": "tool",
                                "id": part["id"],
                                "name": part["name"],
                                "state": item,
                            }
                        )
                group.append(
                    {
                        "id": m["id"],
                        "type": kind,
                        "content": content,
                        "finish": m.get("finish"),
                    }
                )
            elif kind in ("idle", "compaction"):
                group.append(
                    {
                        k: m[k]
                        for k in ("id", "type", "outcome", "error", "status")
                        if k in m
                    }
                )
        terminal = any(x["type"] == "idle" for x in groups.get(self.run, []))
        if terminal or dead:
            self.fenced = True
            self.clean_inbox(self.run)
        # Historical runs have already been published. Include the latest one
        # until this run appears, for its final cumulative usage baseline.
        last = next(reversed(groups), None)
        return {
            "run": last,
            "messages": groups.get(last, []),
            "tokens": export["info"]["tokens"],
            "fenced": self.fenced,
        }

    def snapshot_frame(self, dead=False):
        # AgentBox stdio has a bounded frame size. Keep a large projection in
        # the private session directory and stream it through Sandbox.Read.
        snapshot = self.snapshot(dead=dead)
        payload = json.dumps(snapshot, separators=(",", ":"))
        if len(payload) < 4 * 1024 * 1024:
            return {"snapshot": snapshot}
        ticket = secrets.token_hex(16)
        tmp = self.data / "projection.tmp"
        with tmp.open("w") as out:
            out.write('{"ticket":' + json.dumps(ticket) + ',"snapshot":')
            out.write(payload)
            out.write("}")
        tmp.replace(self.data / "projection.json")
        return {"ticket": ticket}

    def clean_inbox(self, owner):
        for row in self.request(self.path + "/inbox"):
            run = row.get("payload", {}).get("metadata", {}).get("orpheus_run_id")
            if (owner is None and run != self.run) or (
                owner is not None and run == owner
            ):
                if not run:
                    raise Failure("integrity")
                self.request(
                    self.path + "/inbox/" + quote(row["id"], safe=""), "DELETE"
                )

    def deliver(self, p):
        if self.fenced:
            raise Failure("finished")
        inputs = p["inputs"]
        for i, item in enumerate(inputs):
            mid = "msg_" + item["id"]
            meta = {"orpheus_run_id": self.run, "orpheus_operation_id": p["operation"]}
            expected = {"text": item["text"], "metadata": meta}
            if p.get("replay"):
                try:
                    prior = self.request(self.path + "/message/" + quote(mid, safe=""))
                except Failure as err:
                    if err.code != 404:
                        raise
                    prior = None
                if prior:
                    if prior.get("type") != "user" or any(
                        prior.get(k) != v for k, v in expected.items()
                    ):
                        raise Failure("integrity")
                    continue
                queued = next(
                    (x for x in self.request(self.path + "/inbox") if x["id"] == mid),
                    None,
                )
                if queued:
                    if queued["type"] != "user" or queued["payload"] != expected:
                        raise Failure("integrity")
                    continue
            receipt = self.request(
                self.path + "/prompt",
                "POST",
                dict(
                    expected,
                    id=mid,
                    delivery="steer",
                    resume=p["start"] and i == len(inputs) - 1,
                ),
            )
            if (
                receipt["id"] != mid
                or receipt["type"] != "user"
                or receipt["payload"] != expected
            ):
                raise Failure("integrity")

    def call(self, method, p):
        if p["run"] != self.run:
            raise Failure("finished")
        if method == "ready":
            return True
        if method == "seed":
            self.seed(p["entry"], p.get("replaceID", ""))
        elif method == "credential":
            return self.entry(p["provider"])
        elif method == "limits":
            return self.request("/api/experimental/usage/limits")
        elif method == "open":
            if p["existing"]:
                try:
                    self.request(self.path)
                except Failure as err:
                    if err.code == 404:
                        raise Failure("context_lost") from None
                    raise
                self.clean_inbox(None)
            else:
                model = {"providerID": p["provider"], "id": p["model"]}
                if p.get("variant"):
                    model["variant"] = p["variant"]
                self.request(
                    "/api/session",
                    "POST",
                    {
                        "id": self.session,
                        "model": model,
                        "location": {"directory": p["cwd"]},
                        "agent": "build",
                    },
                )
            if p["instructions"]:
                self.request(
                    self.experimental + "/instructions/entries/orpheus-profile",
                    "PUT",
                    {"value": p["instructions"]},
                )
        elif method == "deliver":
            self.deliver(p)
        elif method == "snapshot":
            return self.snapshot_frame()
        elif method == "interrupt":
            return self.request(self.path + "/interrupt?resume=false", "POST")
        else:
            raise Failure("unknown_method")
        return None


def main():
    os.umask(0o077)
    server = Server(sys.argv[1], sys.argv[2])
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
    try:
        if len(sys.argv) > 3:
            server.output(
                server.entry(sys.argv[4])
                if sys.argv[3] == "credential"
                else server.snapshot_frame(dead=True)
            )
            return
        threading.Thread(target=server.events, daemon=True).start()
        for line in sys.stdin:
            request = json.loads(line)
            try:
                result = {"data": server.call(request["method"], request["params"])}
            except Failure as err:
                result = {"failure": err.code}
            except (OSError, KeyError, ValueError, TypeError):
                result = {"failure": "transport"}
            server.output({"id": request["id"], "result": result})
    finally:
        server.close()


if __name__ == "__main__":
    main()
