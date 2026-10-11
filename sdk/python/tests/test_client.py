import base64
import io
import json
import unittest
from unittest.mock import patch
from urllib.error import HTTPError, URLError

from kumabox import Client, CommandExitError, DataDisk, KumaBoxError, Sandbox


class Response(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()


class ClientTest(unittest.TestCase):
    def test_create_serializes_resource_options_and_preserves_false(self):
        response = Response(b'{"id":"vm-1","shared_memory":true}')
        with patch("kumabox.client.urlopen", return_value=response) as send:
            sandbox = Client(token="secret", request_timeout=60).create(
                "ubuntu",
                cpus=2,
                memory=1024**3,
                nics=0,
                start=False,
                shared_memory=True,
                data_disks=[DataDisk("data", 256 * 1024**2, direct_io=False)],
                request_timeout=5,
            )
        request = send.call_args.args[0]
        body = json.loads(request.data)
        self.assertTrue(sandbox.data["shared_memory"])
        self.assertEqual(
            body["data_disks"],
            [{"name": "data", "size": 256 * 1024**2, "fstype": "ext4", "direct_io": False}],
        )
        self.assertEqual(
            (body["cpus"], body["memory"], body["nics"], body["start"]), (2, 1024**3, 0, False)
        )
        self.assertNotIn("request_timeout", body)
        self.assertEqual(send.call_args.kwargs["timeout"], 5)

    def test_command_timeout_and_http_timeout_are_independent(self):
        response = Response(b'{"error":{"code":"COMMAND_TIMEOUT","message":"deadline"}}\n')
        sandbox = Sandbox(Client(token="secret"), {"id": "vm-1"})
        with patch("kumabox.client.urlopen", return_value=response) as send:
            with self.assertRaises(KumaBoxError) as raised:
                sandbox.commands.run("sleep 10", timeout=5, request_timeout=6)
        self.assertEqual(raised.exception.code, "COMMAND_TIMEOUT")
        self.assertEqual(json.loads(send.call_args.args[0].data)["timeout_ms"], 5000)
        self.assertEqual(send.call_args.kwargs["timeout"], 6)

    def test_invalid_timeouts_fail_before_network_io(self):
        with patch("kumabox.client.urlopen") as send:
            for value in [0, -1, float("inf"), float("nan"), 86401, True]:
                with self.subTest(value=value):
                    with self.assertRaises(ValueError):
                        Client(token="secret", request_timeout=value)
                    sandbox = Sandbox(Client(token="secret"), {"id": "vm-1"})
                    with self.assertRaises(ValueError):
                        sandbox.commands.run("true", timeout=value)
                    with self.assertRaises(ValueError):
                        sandbox.files.read("/tmp/file", request_timeout=value)
            send.assert_not_called()

    def test_transport_errors_are_typed_without_retrying(self):
        for error, code in [
            (TimeoutError(), "REQUEST_TIMEOUT"),
            (URLError(TimeoutError()), "REQUEST_TIMEOUT"),
            (URLError("connection refused"), "CONNECTION_ERROR"),
        ]:
            with self.subTest(code=code, error=error):
                with patch("kumabox.client.urlopen", side_effect=error) as send:
                    with self.assertRaises(KumaBoxError) as raised:
                        Client(token="secret").create("ubuntu")
                self.assertEqual(raised.exception.code, code)
                self.assertEqual(send.call_count, 1)

    def test_http_errors_preserve_partial_commit_and_close_response(self):
        body = io.BytesIO(
            b'{"error":{"code":"FAILED","message":"retained","committed":true,"resource_id":"vm-1"}}'
        )
        error = HTTPError("http://localhost", 500, "failure", {}, body)
        with patch("kumabox.client.urlopen", side_effect=error):
            with self.assertRaises(KumaBoxError) as raised:
                Client(token="secret").create("ubuntu")
        self.assertTrue(raised.exception.committed)
        self.assertEqual(raised.exception.resource_id, "vm-1")
        self.assertTrue(body.closed)

    def test_callback_timeouterror_is_not_mistaken_for_transport_timeout(self):
        def fail(_):
            raise TimeoutError("user callback")

        response = Response(b'{"stream":"stdout","data":"aGk="}\n')
        with patch("kumabox.client.urlopen", return_value=response):
            sandbox = Sandbox(Client(token="secret"), {"id": "vm-1"})
            with self.assertRaisesRegex(TimeoutError, "user callback"):
                sandbox.commands.run("echo", on_stdout=fail)
        self.assertTrue(response.closed)

    def test_callbacks_stream_complete_unicode_before_exit(self):
        stdout, stderr = [], []
        encoded = "你🙂".encode()
        frames = [
            {"stream": "stdout", "data": base64.b64encode(encoded[:1]).decode()},
            {"stream": "stderr", "data": base64.b64encode(b"warning").decode()},
            {"stream": "stdout", "data": base64.b64encode(encoded[1:4]).decode()},
            {"stream": "stdout", "data": base64.b64encode(encoded[4:]).decode()},
            {"exit_code": 0},
        ]

        class StreamingResponse(Response):
            def __next__(self):
                line = super().__next__()
                if b'"exit_code"' in line:
                    self_test.assertEqual("".join(stdout), "你🙂")
                    self_test.assertEqual("".join(stderr), "warning")
                return line

        self_test = self
        response = StreamingResponse(
            b"".join(json.dumps(frame).encode() + b"\n" for frame in frames)
        )
        with patch("kumabox.client.urlopen", return_value=response):
            sandbox = Sandbox(Client(token="secret"), {"id": "vm-1"})
            result = sandbox.commands.run("echo", on_stdout=stdout.append, on_stderr=stderr.append)
        self.assertEqual(result.stdout, "你🙂")
        self.assertEqual(result.stderr, "warning")
        self.assertTrue(response.closed)

    def test_callback_failure_closes_response(self):
        response = Response(b'{"stream":"stdout","data":"aGk="}\n')

        def fail(_):
            raise RuntimeError("callback failed")

        with patch("kumabox.client.urlopen", return_value=response):
            sandbox = Sandbox(Client(token="secret"), {"id": "vm-1"})
            with self.assertRaisesRegex(RuntimeError, "callback failed"):
                sandbox.commands.run("echo", on_stdout=fail)
        self.assertTrue(response.closed)

    def test_callbacks_flush_incomplete_unicode_on_nonzero_exit(self):
        response = Response(b'{"stream":"stderr","data":"4g=="}\n{"exit_code":7}\n')
        output = []
        with patch("kumabox.client.urlopen", return_value=response):
            sandbox = Sandbox(Client(token="secret"), {"id": "vm-1"})
            with self.assertRaises(CommandExitError) as raised:
                sandbox.commands.run("false", on_stderr=output.append)
        self.assertEqual("".join(output), "\ufffd")
        self.assertEqual(raised.exception.stderr, "\ufffd")

    def test_missing_exit_is_not_success(self):
        response = Response(b'{"stream":"stdout","data":"aGk="}\n')
        with patch("kumabox.client.urlopen", return_value=response):
            sandbox = Sandbox(Client(token="secret"), {"id": "vm-1"})
            with self.assertRaises(KumaBoxError) as raised:
                sandbox.commands.run("echo")
        self.assertEqual(raised.exception.code, "TRUNCATED_STREAM")
        self.assertTrue(response.closed)

    def test_files_transfer_text_and_binary(self):
        replies = [
            Response(b'{"name":"sample.bin","path":"/tmp/sample.bin"}'),
            Response(bytes([0, 1, 255])),
        ]
        with patch("kumabox.client.urlopen", side_effect=replies) as send:
            sandbox = Sandbox(Client(token="secret"), {"id": "vm-1"})
            info = sandbox.files.write("/tmp/sample.bin", bytes([0, 1, 255]))
            data = sandbox.files.read("/tmp/sample.bin", format="bytes")
        self.assertEqual(info["path"], "/tmp/sample.bin")
        self.assertEqual(data, bytes([0, 1, 255]))
        self.assertEqual(send.call_args_list[0].args[0].data, bytes([0, 1, 255]))

    def test_create_and_stream_command(self):
        frames = [
            {"stream": "stdout", "data": "aGVsbG8="},
            {"stream": "stderr", "data": "d2FybmluZw=="},
            {"exit_code": 7},
        ]
        replies = [
            Response(json.dumps({"id": "vm-1", "state": "running"}).encode()),
            Response(b"".join(json.dumps(frame).encode() + b"\n" for frame in frames)),
        ]
        with patch("kumabox.client.urlopen", side_effect=replies) as send:
            client = Client(token="secret")
            sandbox = client.create("ubuntu", name="test")
            result = sandbox.commands.run("echo hello", check=False)
        self.assertEqual(sandbox.sandbox_id, "vm-1")
        self.assertEqual((result.stdout, result.stderr, result.exit_code), ("hello", "warning", 7))
        create_body = json.loads(send.call_args_list[0].args[0].data)
        command_body = json.loads(send.call_args_list[1].args[0].data)
        self.assertEqual(create_body["image"], "ubuntu")
        self.assertEqual(command_body["args"], ["sh", "-lc", "echo hello"])

    def test_stream_failure_is_typed(self):
        frames = Response(b'{"error":{"code":"ARTIFACT_UNAVAILABLE","message":"guest stopped"}}\n')
        with patch("kumabox.client.urlopen", return_value=frames):
            sandbox = Sandbox(Client(token="secret"), {"id": "vm-1"})
            with self.assertRaises(KumaBoxError) as raised:
                sandbox.commands.run(["true"])
        self.assertEqual(raised.exception.code, "ARTIFACT_UNAVAILABLE")

    def test_nonzero_exit_raises_with_output(self):
        frames = Response(b'{"stream":"stderr","data":"YmFk"}\n{"exit_code":7}\n')
        with patch("kumabox.client.urlopen", return_value=frames):
            sandbox = Sandbox(Client(token="secret"), {"id": "vm-1"})
            with self.assertRaises(CommandExitError) as raised:
                sandbox.commands.run("false")
        self.assertEqual((raised.exception.exit_code, raised.exception.stderr), (7, "bad"))


if __name__ == "__main__":
    unittest.main()
