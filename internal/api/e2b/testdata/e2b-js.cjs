const assert = require('node:assert/strict')
const { Sandbox } = require(process.env.KUMABOX_E2B_NODE_MODULE)

async function main() {
  const metadata = { 'team +&%': 'infra /+&%' }
  const sandbox = await Sandbox.create('ubuntu', { metadata, envs: { DEFAULT: 'base', OVERRIDE: 'base' } })
  assert.equal(sandbox.sandboxId, 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa')
  const result = await sandbox.commands.run('echo hello', { envs: { OVERRIDE: 'command' } })
  assert.equal(result.stdout, 'hello')
  assert.equal(result.stderr, 'warning')
  const info = await sandbox.getInfo()
  assert.deepEqual(info.metadata, metadata)
  assert.equal(info.templateId, 'ubuntu')
  const paginator = Sandbox.list({ query: { metadata, state: ['running', 'paused'], template: 'ubuntu' }, limit: 1 })
  const listed = await paginator.nextItems()
  assert.equal(listed.length, 1)
  assert.equal(listed[0].sandboxId, sandbox.sandboxId)
  assert.deepEqual(listed[0].metadata, metadata)
  assert.equal(paginator.hasNext, false)
  const same = await Sandbox.connect(sandbox.sandboxId)
  assert.equal(same.sandboxId, sandbox.sandboxId)
  await same.commands.run('echo hello')
  const snapshot = await sandbox.createSnapshot({ name: 'warm' })
  const clone = await Sandbox.create(snapshot.snapshotId)
  assert.equal(clone.sandboxId, sandbox.sandboxId)
  assert.equal(await sandbox.kill(), true)
}

main().catch(error => {
  console.error(error)
  process.exitCode = 1
})
