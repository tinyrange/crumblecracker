"""Exercise a packaged NeurodeskAppX backend without downloading or booting a VM."""
import json
from pathlib import Path
import queue
import subprocess
import sys
import tempfile
import threading
import urllib.error
import urllib.parse
import urllib.request
import uuid


def check(binary, cache, shutdown):
    with tempfile.TemporaryFile(mode="w+") as errors:
        process = subprocess.Popen(
            [str(binary), "--headless", "--cache-dir", str(cache)],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=errors, text=True,
        )
        try:
            lines = queue.Queue()
            threading.Thread(target=lambda: lines.put(process.stdout.readline()), daemon=True).start()
            ready = json.loads(lines.get(timeout=30))
            assert ready["event"] == "ready" and ready["protocol"] == "ndappx"
            assert ready["api_version"] == 1 and len(ready["token"]) == 43
            address = urllib.parse.urlsplit(ready["base_url"])
            assert address.scheme == "http" and address.hostname == "127.0.0.1" and address.port

            def request(path, body=None, authorized=True):
                headers = {"Content-Type": "application/json", "Idempotency-Key": str(uuid.uuid4())}
                if authorized:
                    headers["Authorization"] = "Bearer " + ready["token"]
                req = urllib.request.Request(ready["base_url"] + path,
                    data=None if body is None else json.dumps(body).encode(), headers=headers)
                with urllib.request.urlopen(req, timeout=15) as response:
                    return json.load(response)

            try:
                request("/v1/info", authorized=False)
                raise AssertionError("unauthenticated request accepted")
            except urllib.error.HTTPError as error:
                assert error.code == 401
            request("/v1/info")
            capability = request("/v1/virtualization")
            print("Virtualization:", json.dumps(capability), flush=True)
            if shutdown:
                request("/v1/shutdown", {})
            else:
                process.stdin.close()
            assert process.wait(timeout=30) == 0
            print("PASS: packaged backend auth, readiness and " + ("HTTP shutdown" if shutdown else "stdin EOF shutdown"), flush=True)
        except BaseException:
            errors.seek(0)
            sys.stderr.write(errors.read())
            raise
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()
            if not process.stdin.closed:
                process.stdin.close()
            process.stdout.close()


if __name__ == "__main__":
    binary = Path(sys.argv[1]).resolve()
    with tempfile.TemporaryDirectory(prefix="crumblecracker-release-smoke-") as directory:
        for shutdown in [True, False]:
            check(binary, Path(directory) / "cache", shutdown)
