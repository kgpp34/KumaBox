"""Exercise initramfs identity setup without mounting filesystems or using KVM."""

import os
from pathlib import Path
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


if __name__ == "__main__":
    unittest.main()
