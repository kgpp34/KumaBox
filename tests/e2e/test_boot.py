"""Exercise guest file preparation without mounting filesystems or KVM.

Native identity, disk discovery and network tests live alongside kumabox-init.
"""

import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile
import unittest


REPOSITORY = Path(__file__).resolve().parents[2]


@unittest.skipUnless(shutil.which("systemd-tmpfiles"), "requires systemd-tmpfiles")
class BootFilesTests(unittest.TestCase):
    def prepare(self, root, unit):
        lines = (REPOSITORY / "oci-images/base/24.04" / unit).read_text().splitlines()
        commands = [line.removeprefix("ExecStart=") for line in lines
                    if line.startswith("ExecStart=") and line != "ExecStart="]
        self.assertEqual(len(commands), 1)
        command = shlex.split(commands[0])
        subprocess.run(
            [command[0], "--root=" + str(root), *command[1:]], check=True,
            capture_output=True,
        )

    def test_late_journal_preparation_preserves_commands_and_uploads(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            rules = root / "etc/tmpfiles.d"
            rules.mkdir(parents=True)
            uid, gid = os.getuid(), os.getgid()
            (rules / "test.conf").write_text(
                f"D /tmp 1777 {uid} {gid} -\n"
                f"F! /tmp/prepared 0600 {uid} {gid} - prepared\n"
                f"d /var/log/journal 2755 {uid} {gid} -\n"
                f"f /var/log/journal/test.journal 0640 {uid} {gid} - journal\n"
            )
            (root / "tmp").mkdir()
            (root / "tmp/stale").write_text("previous boot")
            self.prepare(root, "kumabox-guest-prepare.service")
            self.assertFalse((root / "tmp/stale").exists())
            self.assertEqual((root / "tmp/prepared").read_text(), "prepared")
            self.assertFalse((root / "var/log/journal/test.journal").exists())

            (root / "tmp/prepared").write_text("command output")
            (root / "tmp/upload").write_bytes(b"uploaded\x00\xff")
            self.prepare(root, "systemd-tmpfiles-setup.conf")
            self.assertEqual((root / "tmp/prepared").read_text(), "command output")
            self.assertEqual((root / "tmp/upload").read_bytes(), b"uploaded\x00\xff")
            self.assertEqual((root / "var/log/journal/test.journal").read_text(), "journal")


if __name__ == "__main__":
    unittest.main()
