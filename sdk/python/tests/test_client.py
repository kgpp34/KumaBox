import io
import json
import unittest
from unittest.mock import patch

from kumabox import Client, CommandExitError, KumaBoxError, Sandbox


class Response(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()


class ClientTest(unittest.TestCase):
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
