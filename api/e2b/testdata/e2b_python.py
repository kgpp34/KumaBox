from e2b import Sandbox, SandboxQuery, SandboxState


metadata = {"team +&%": "infra /+&%"}
sandbox = Sandbox.create("ubuntu", metadata=metadata, envs={"DEFAULT": "base", "OVERRIDE": "base"})
assert sandbox.sandbox_id == "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
result = sandbox.commands.run("echo hello", envs={"OVERRIDE": "command"})
assert result.stdout == "hello", result
assert result.stderr == "warning", result
info = sandbox.get_info()
assert info.metadata == metadata, info
assert info.template_id == "ubuntu", info
paginator = Sandbox.list(query=SandboxQuery(metadata=metadata, state=[SandboxState.RUNNING, SandboxState.PAUSED], template="ubuntu"), limit=1)
listed = paginator.next_items()
assert len(listed) == 1, listed
assert listed[0].sandbox_id == sandbox.sandbox_id, listed
assert listed[0].metadata == metadata, listed
assert not paginator.has_next
sandbox.connect()
sandbox.commands.run("echo hello")
snapshot = sandbox.create_snapshot(name="warm")
clone = Sandbox.create(snapshot.snapshot_id)
assert clone.sandbox_id == sandbox.sandbox_id
assert sandbox.kill()
