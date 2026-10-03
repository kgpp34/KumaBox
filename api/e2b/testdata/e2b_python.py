from e2b import Sandbox


sandbox = Sandbox.create("ubuntu")
assert sandbox.sandbox_id == "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
result = sandbox.commands.run("echo hello")
assert result.stdout == "hello", result
assert result.stderr == "warning", result
sandbox.connect()
snapshot = sandbox.create_snapshot(name="warm")
clone = Sandbox.create(snapshot.snapshot_id)
assert clone.sandbox_id == sandbox.sandbox_id
assert sandbox.kill()
