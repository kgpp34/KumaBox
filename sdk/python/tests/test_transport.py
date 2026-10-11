"""Exercise socket read timeouts through real HTTP response bodies."""

import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from kumabox import Client, KumaBoxError, Sandbox


class TransportTest(unittest.TestCase):
    def test_timeouts_cover_headers_json_files_and_exec(self):
        release = threading.Event()
        paths = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                paths.append(self.path)
                if self.path.endswith("no-headers"):
                    release.wait(5)
                    return
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b"[")
                self.wfile.flush()
                release.wait(5)

            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                paths.append(self.path)
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"stream":"stdout","data":"aGk="}\n')
                self.wfile.flush()
                release.wait(5)

            def log_message(self, *_):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(
            target=lambda: server.serve_forever(poll_interval=0.01), daemon=True
        )
        thread.start()
        client = Client(
            base_url=f"http://127.0.0.1:{server.server_port}", token="secret", request_timeout=0.2
        )
        sandbox = Sandbox(client, {"id": "vm-1"})
        try:
            for name, run in [
                ("headers", lambda: client.connect("no-headers")),
                ("JSON", client.list),
                ("file", lambda: sandbox.files.read("/tmp/file")),
                ("exec", lambda: sandbox.commands.run("sleep 10")),
            ]:
                with self.subTest(name=name):
                    with self.assertRaises(KumaBoxError) as raised:
                        run()
                    self.assertEqual(raised.exception.code, "REQUEST_TIMEOUT")
            self.assertEqual(len(paths), 4, "timed-out operations must not be retried")
        finally:
            release.set()
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)
        self.assertFalse(thread.is_alive())


if __name__ == "__main__":
    unittest.main()
