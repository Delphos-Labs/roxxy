import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[1]


class ModuleIdentityTest(unittest.TestCase):
    def test_canonical_module_identity(self):
        result = subprocess.run(
            ["go", "list", "-m"], cwd=ROOT, capture_output=True, text=True, check=True,
        )
        self.assertEqual(result.stdout.strip(), "github.com/Delphos-Labs/roxxy")

    def test_legacy_module_path_does_not_return(self):
        result = subprocess.run(
            ["git", "grep", "-l", "-F", "github.com/" + "delphos/roxxy", "--"],
            cwd=ROOT, capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)


class ReleaseChecksumsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="roxxy-checksums-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.dist = self.root / "dist"
        self.dist.mkdir()
        self.assets = {
            "roxxy-linux-amd64.tar.gz": b"linux release fixture",
            "roxxy-darwin-arm64.zip": b"macOS release fixture",
            "-release-metadata.txt": b"leading-dash filename fixture",
        }
        for name, content in self.assets.items():
            (self.dist / name).write_bytes(content)

    def generate(self):
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        _, marker, step = workflow.partition("      - name: Generate release notes\n")
        self.assertTrue(marker, "Release notes step not found. Update the checksum test's workflow locator.")
        _, marker, script = step.partition("        run: |\n")
        self.assertTrue(marker, "Release notes step must contain a shell run block.")
        commands, marker, _ = script.partition("          cat > release_notes.md")
        self.assertTrue(marker, "Release notes heredoc not found. Update the checksum test's script boundary.")
        subprocess.run(
            ["bash", "-euo", "pipefail", "-c", textwrap.dedent(commands)],
            cwd=self.root, check=True, env={**os.environ, "GITHUB_REF_NAME": "v1.2.3"},
        )

    def verify(self, directory, *options):
        return subprocess.run(
            ["sha256sum", "--check", *options, "SHA256SUMS"],
            cwd=directory, capture_output=True, text=True,
        )

    def test_flat_downloads_match_independent_hashes(self):
        self.generate()
        manifest = (self.dist / "SHA256SUMS").read_text()
        expected = "".join(
            f"{hashlib.sha256(content).hexdigest()}  {name}\n"
            for name, content in sorted(self.assets.items())
        )
        self.assertEqual(manifest, expected)
        result = self.verify(self.dist)
        self.assertEqual(result.returncode, 0, result.stderr)
        (self.dist / next(iter(self.assets))).write_bytes(b"tampered")
        self.assertNotEqual(self.verify(self.dist).returncode, 0)

    def test_selected_platform_download(self):
        self.generate()
        download = self.root / "download"
        download.mkdir()
        (download / "SHA256SUMS").write_bytes((self.dist / "SHA256SUMS").read_bytes())
        name, content = next(iter(self.assets.items()))
        (download / name).write_bytes(content)
        result = self.verify(download, "--ignore-missing")
        self.assertEqual(result.returncode, 0, result.stderr)
        (download / name).unlink()
        self.assertNotEqual(self.verify(download, "--ignore-missing").returncode, 0)


if __name__ == "__main__":
    unittest.main()
