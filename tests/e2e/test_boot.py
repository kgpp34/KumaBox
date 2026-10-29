"""Exercise boot identity and file preparation without mounting filesystems or KVM."""

import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile
import unittest


REPOSITORY = Path(__file__).resolve().parents[2]


class BootIdentityTests(unittest.TestCase):
    def test_first_boot_clears_image_identity_and_restart_preserves_private_identity(self):
        profiles = (
            ("fastboot", "oci-images/fastboot/init",
             "if [ ! -s /cow/upper/etc/machine-id ]; then", "\nfi"),
            ("ubuntu", "oci-images/ubuntu/overlay.sh",
             'if [ ! -s "$workspace/cow/upper/etc/machine-id" ]; then', "\n\tfi"),
        )
        for profile, filename, beginning, ending in profiles:
            source = (REPOSITORY / filename).read_text()
            start = source.index(beginning)
            end = source.index(ending, start) + len(ending)
            # Execute the real shell block, replacing only the fastboot mount paths.
            block = source[start:end]
            for state in ("image-only", "empty-private", "existing-private"):
                with self.subTest(profile=profile, state=state), tempfile.TemporaryDirectory() as temp:
                    base = Path(temp)
                    upper = base / "cow/upper/etc"
                    target = base / "root/etc"
                    upper.mkdir(parents=True)
                    target.mkdir(parents=True)
                    (target / "machine-id").write_text("baked-image-id\n")
                    if state == "empty-private":
                        (upper / "machine-id").touch()
                    elif state == "existing-private":
                        (upper / "machine-id").write_text("private-id\n")
                        (target / "machine-id").write_text("private-id\n")
                    script = block
                    if profile == "fastboot":
                        script = script.replace("/cow/upper/", str(base / "cow/upper") + "/")
                        script = script.replace("/newroot/", str(base / "root") + "/")
                    subprocess.run(
                        ["/bin/sh", "-eu", "-c", script], check=True,
                        env={**os.environ, "workspace": str(base), "rootmnt": str(base / "root")},
                    )
                    expected = "private-id\n" if state == "existing-private" else ""
                    self.assertEqual((target / "machine-id").read_text(), expected)


@unittest.skipUnless(shutil.which("systemd-tmpfiles"), "requires systemd-tmpfiles")
class BootFilesTests(unittest.TestCase):
    def prepare(self, root, unit):
        lines = (REPOSITORY / "oci-images/fastboot" / unit).read_text().splitlines()
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
