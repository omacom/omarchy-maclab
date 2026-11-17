import hashlib
import json
import os
import re
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).with_name("module-build.py")
KBUILD = Path(__file__).with_name("kbuild")


class ModuleBuildTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.src = self.root / "src"
        self.obj = self.root / "obj"
        self.io = self.root / "io"
        self.inp = self.root / "in"
        for p in (self.src, self.obj, self.io, self.inp):
            p.mkdir()
        self.put(
            self.src,
            "Makefile",
            'srctree := $(CURDIR)\nrun-command:\n\tcd "$(O)" && srctree="$(CURDIR)" sh $(KBUILD_RUN_COMMAND)\n',
        )
        self.put(self.src, "include/linux/module.h", "/* fixture source header */\n")
        # This fixture substitutes only the kernel-owned header installer.
        self.put(
            self.src,
            "scripts/package/install-extmod-build",
            """set -eu
mkdir -p "$1/include/linux" "$1/include/generated" "$1/scripts/mod"
cp "$srctree/Makefile" "$1/Makefile"
cp "$srctree/include/linux/module.h" "$1/include/linux/module.h"
cp Module.symvers "$1/Module.symvers"
cp include/generated/* "$1/include/generated/"
printf 'fixture host modpost' > "$1/scripts/mod/modpost"
""",
        )
        self.put(self.obj, ".config", 'CONFIG_CC_VERSION_TEXT="fixture compiler"\n')
        self.put(
            self.obj,
            "Module.symvers",
            "0x0\tfixture_export\tvmlinux\tEXPORT_SYMBOL\t\n",
        )
        self.put(self.obj, "vmlinux", "fixture ELF bytes, not a built kernel\n")
        self.put(self.obj, "include/config/kernel.release", "future-test-release\n")
        self.put(self.obj, "include/config/auto.conf", "CONFIG_MODULES=y\n")
        self.put(
            self.obj,
            "include/generated/compile.h",
            '#define LINUX_COMPILER\t\t"fixture cc and linker"\n',
        )
        self.put(self.obj, "include/generated/autoconf.h", "#define CONFIG_MODULES 1\n")
        self.put(
            self.obj,
            "include/generated/utsrelease.h",
            '#define UTS_RELEASE "future-test-release"\n',
        )
        self.put(self.inp, "config", 'CONFIG_LOCALVERSION="input"\n')
        subprocess.run(["git", "init", "-q", str(self.src)], check=True)
        subprocess.run(["git", "-C", str(self.src), "add", "."], check=True)
        subprocess.run(
            [
                "git", "-C", str(self.src),
                "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid",
                "commit", "-qm", "fixture",
            ],
            check=True,
        )
        self.sha = subprocess.check_output(
            ["git", "-C", str(self.src), "rev-parse", "HEAD"], text=True
        ).strip()

    def put(self, root, name, text):
        p = root / name
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(text)

    def run_pack(self, **extra):
        env = dict(os.environ, SHA=self.sha, URL="https://example.invalid/kernel.git")
        env.update(extra)
        return subprocess.run(
            [
                "python3",
                str(SCRIPT),
                str(self.src),
                str(self.obj),
                str(self.inp / "config"),
                str(self.io),
                str(self.src / ".git"),
            ],
            env=env,
            text=True,
            capture_output=True,
            check=False,
        )

    def test_archive_retains_exact_build_and_identity(self):
        r = self.run_pack()
        self.assertEqual(r.returncode, 0, r.stderr)
        archive = self.io / "out/module-build.tar.zst"
        raw = subprocess.check_output(["zstd", "-dq", "-c", str(archive)])
        tar_path = self.root / "artifact.tar"
        tar_path.write_bytes(raw)
        with tarfile.open(tar_path) as t:
            identity = json.load(t.extractfile("IDENTITY.json"))
            self.assertEqual(identity["source_commit"], self.sha)
            self.assertEqual(
                identity["source_repo"], "https://example.invalid/kernel.git"
            )
            self.assertEqual(identity["release"], "future-test-release")
            self.assertEqual(
                identity["linux_compiler"],
                '#define LINUX_COMPILER\t\t"fixture cc and linker"',
            )
            for path, expected in [
                ("build/Module.symvers", (self.obj / "Module.symvers").read_bytes()),
                ("build/vmlinux", (self.obj / "vmlinux").read_bytes()),
                ("build/.config", (self.obj / ".config").read_bytes()),
                ("input.config", (self.inp / "config").read_bytes()),
                (
                    "build/include/linux/module.h",
                    (self.src / "include/linux/module.h").read_bytes(),
                ),
                (
                    "build/include/generated/autoconf.h",
                    (self.obj / "include/generated/autoconf.h").read_bytes(),
                ),
            ]:
                self.assertEqual(t.extractfile(path).read(), expected)
                self.assertEqual(
                    identity["files"][path]["sha256"],
                    hashlib.sha256(expected).hexdigest(),
                )
            self.assertNotEqual(
                identity["files"]["input.config"]["sha256"],
                identity["files"]["build/.config"]["sha256"],
            )

    def test_kbuild_package_stage_finds_the_config_lab_builder_put_in(self):
        # lab-builder mounts the build's input read-only at /in and an empty
        # /io for outputs: the config the build used is /in/config, never /io/config.
        inp = self.inp
        (self.root / "git").symlink_to(self.src / ".git")
        lines = [l for l in KBUILD.read_text().splitlines() if "module-build.py" in l]
        self.assertEqual(len(lines), 1, lines)
        line = lines[0].replace("/usr/local/bin/module-build.py", str(SCRIPT))
        line = re.sub(r"(?<![\w$/])/in\b", str(inp), line)
        line = re.sub(r"(?<![\w$/])/io\b", str(self.io), line)
        env = dict(os.environ, SHA=self.sha, URL="https://example.invalid/kernel.git",
                   C=str(self.root), O=str(self.obj))
        r = subprocess.run(["bash", "-c", line], env=env, text=True, capture_output=True)
        self.assertEqual(r.returncode, 0, r.stderr)
        archive = self.io / "out/module-build.tar.zst"
        raw = subprocess.check_output(["zstd", "-dq", "-c", str(archive)])
        tar_path = self.root / "artifact.tar"
        tar_path.write_bytes(raw)
        with tarfile.open(tar_path) as t:
            self.assertEqual(t.extractfile("input.config").read(), (inp / "config").read_bytes())

    def test_space_separated_compiler_identity_still_packages(self):
        self.put(self.obj, "include/generated/compile.h",
                 '#define LINUX_COMPILER "fixture cc and linker"\n')
        r = self.run_pack()
        self.assertEqual(r.returncode, 0, r.stderr)

    def test_missing_or_ambiguous_compiler_identity_refuses(self):
        for text in [
            '#define LINUX_COMPILE_HOST "fixture"\n',
            '#define LINUX_COMPILER\t\t\n',
            '#define LINUX_COMPILER\t"one"\n#define LINUX_COMPILER "two"\n',
            '#define LINUX_COMPILER_EXTRA "lookalike"\n',
        ]:
            with self.subTest(header=text):
                (self.io / "out/module-build.tar.zst").unlink(missing_ok=True)
                self.put(self.obj, "include/generated/compile.h", text)
                r = self.run_pack()
                self.assertNotEqual(r.returncode, 0)
                self.assertIn("missing compiled release/compiler identity", r.stderr)
                self.assertFalse((self.io / "out/module-build.tar.zst").exists())

    def test_native_staging_can_publish_to_another_filesystem(self):
        shared = Path("/dev/shm")
        if not shared.is_dir() or shared.stat().st_dev == self.obj.stat().st_dev:
            self.skipTest("a second writable filesystem is unavailable")
        with tempfile.TemporaryDirectory(dir=shared) as output:
            self.io = Path(output)
            r = self.run_pack()
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertTrue((self.io / "out/module-build.tar.zst").is_file())
            self.assertEqual(list((self.io / "out").glob(".module-build-*")), [])

    def test_missing_export_table_refuses_without_artifact(self):
        (self.obj / "Module.symvers").unlink()
        r = self.run_pack()
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("Module.symvers", r.stderr)
        self.assertFalse((self.io / "out/module-build.tar.zst").exists())

    def test_empty_export_table_refuses_without_artifact(self):
        (self.obj / "Module.symvers").write_bytes(b"")
        r = self.run_pack()
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("Module.symvers", r.stderr)
        self.assertFalse((self.io / "out/module-build.tar.zst").exists())

    def test_wrong_source_identity_refuses(self):
        r = self.run_pack(SHA="0" * 40)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("source commit mismatch", r.stderr)
        self.assertFalse((self.io / "out/module-build.tar.zst").exists())

    def test_changed_source_refuses(self):
        self.put(self.src, "include/linux/module.h", "changed source\n")
        r = self.run_pack()
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("source has tracked changes", r.stderr)

    def test_missing_generated_header_refuses(self):
        (self.obj / "include/generated/autoconf.h").unlink()
        r = self.run_pack()
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("autoconf.h", r.stderr)


if __name__ == "__main__":
    unittest.main()
