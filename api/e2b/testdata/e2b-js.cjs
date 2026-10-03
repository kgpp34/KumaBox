const assert = require('node:assert/strict')
const { Sandbox } = require(process.env.KUMABOX_E2B_NODE_MODULE)

async function main() {
  const sandbox = await Sandbox.create('ubuntu')
  assert.equal(sandbox.sandboxId, 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa')
  const result = await sandbox.commands.run('echo hello')
  assert.equal(result.stdout, 'hello')
  assert.equal(result.stderr, 'warning')
  const same = await Sandbox.connect(sandbox.sandboxId)
  assert.equal(same.sandboxId, sandbox.sandboxId)
  const snapshot = await sandbox.createSnapshot({ name: 'warm' })
  const clone = await Sandbox.create(snapshot.snapshotId)
  assert.equal(clone.sandboxId, sandbox.sandboxId)
  assert.equal(await sandbox.kill(), true)
}

main().catch(error => {
  console.error(error)
  process.exitCode = 1
})
