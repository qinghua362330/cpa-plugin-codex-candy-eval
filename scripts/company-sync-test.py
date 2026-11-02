#!/usr/bin/env python3
"""Offline tests in disposable git repositories; never mutate the working repo."""
import importlib.util
import os
from pathlib import Path
import tempfile
import sys
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("company_sync", Path(__file__).with_name("company-sync.py"))
sync = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sync)


class VersionTests(unittest.TestCase):
    def test_exact_replacement(self):
        source = 'package plugin\nconst (\n\tpluginVersion = "0.4.4-company.1"\n)\n'
        new = sync.company_version("0.4.5", "20261102000000", "123", "2")
        self.assertEqual(sync.replace_version(source, new), source.replace("0.4.4-company.1", new))
        self.assertTrue(sync.COMPANY.fullmatch("v" + new))
        self.assertTrue(sync.COMPANY.fullmatch("v0.4.4-company.1"))

    def test_fail_closed_versions(self):
        for text in ('', 'pluginVersion = "1"\npluginVersion = "2"'):
            with self.assertRaises(ValueError):
                sync.version(text)
        for upstream in ("0.4.4-company.1", "oops", "1.2.3;exit"):
            with self.assertRaises(ValueError):
                sync.company_version(upstream, "20261102", "1", "1")
        self.assertFalse(sync.COMPANY.fullmatch("v0.4.4"))


class MergeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.previous = Path.cwd()
        repo = Path(self.temp.name) / "repo"
        repo.mkdir()
        os.chdir(repo)
        self.env = patch.dict(os.environ, {"RUNNER_TEMP": self.temp.name,
                                          "GITHUB_OUTPUT": str(Path(self.temp.name) / "output")})
        self.env.start()
        sync.git("init", "-b", "main")
        sync.git("config", "user.name", "Test")
        sync.git("config", "user.email", "test@example.invalid")
        Path(sync.SOURCE).parent.mkdir(parents=True)
        Path(sync.SOURCE).write_text('package plugin\nconst (\npluginVersion = "0.4.4"\n)\n')
        with Path(sync.SOURCE).open("a") as stream:
            stream.write('\nfunc implementation() string { return "base" }\n')
        self.commit("base")
        sync.git("branch", "upstream")

    def tearDown(self):
        self.env.stop()
        os.chdir(self.previous)
        self.temp.cleanup()

    def commit(self, message):
        sync.git("add", ".")
        sync.git("commit", "-m", message)

    def test_clean_merge_preserves_custom_commits(self):
        Path("custom").write_text("company feature")
        self.commit("custom")
        custom = sync.git("rev-parse", "HEAD")
        sync.git("checkout", "upstream")
        Path("feature").write_text("upstream feature")
        self.commit("feature")
        sync.git("checkout", "main")
        sync.merge("upstream")
        self.assertEqual(sync.git("rev-parse", "HEAD"), custom)
        self.assertEqual(Path("custom").read_text(), "company feature")
        self.assertEqual(Path("feature").read_text(), "upstream feature")
        sync.git("merge", "--abort")

    def test_version_conflict_aborts_without_changing_main(self):
        path = Path(sync.SOURCE)
        path.write_text(sync.replace_version(path.read_text(), "0.4.4-company.1"))
        self.commit("company")
        before = sync.git("rev-parse", "HEAD")
        sync.git("checkout", "upstream")
        path.write_text(sync.replace_version(path.read_text(), "0.4.5"))
        self.commit("upstream version")
        sync.git("checkout", "main")
        with self.assertRaises(RuntimeError):
            sync.merge("upstream")
        self.assertEqual(sync.git("rev-parse", "HEAD"), before)
        self.assertEqual(sync.version(path.read_text()), "0.4.4-company.1")
        self.assertIn(sync.SOURCE, sync.report_path().read_text())
        self.assertEqual(sync.git("diff", "--name-only", "--diff-filter=U"), "")

    def test_atomic_push_rejects_concurrent_main_without_tag(self):
        remote = Path(self.temp.name) / "remote.git"
        sync.git("init", "--bare", str(remote))
        sync.git("remote", "add", "origin", str(remote))
        sync.git("push", "origin", "main")
        original = sync.git("rev-parse", "HEAD")
        Path("advance").write_text("other contributor")
        sync.git("add", "advance")
        sync.git("commit", "-m", "advance")
        sync.git("push", "origin", "main")
        advanced = sync.git("rev-parse", "HEAD")
        sync.git("checkout", "--detach", original)
        Path("candidate").write_text("candidate")
        sync.git("add", "candidate")
        sync.git("commit", "-m", "candidate")
        sync.git("tag", "v0.4.4-company.1")
        result = sync.run("git", "push", "--atomic", "origin", "HEAD:refs/heads/main",
                          "refs/tags/v0.4.4-company.1", check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(sync.git("--git-dir", str(remote), "rev-parse", "main"), advanced)
        self.assertEqual(sync.git("--git-dir", str(remote), "tag", "--list"), "")

    def test_prepare_noop_then_full_publish(self):
        remote = str(Path(self.temp.name) / "origin.git")
        upstream = str(Path(self.temp.name) / "upstream.git")
        for target in (remote, upstream):
            sync.git("init", "--bare", target)
            sync.git("push", target, "main")
        sync.git("remote", "add", "origin", remote)
        Path("custom").write_text("custom survives")
        source = Path(sync.SOURCE)
        source.write_text(sync.replace_version(source.read_text(), "0.4.4-company.1"))
        self.commit("custom feature and version")
        sync.git("push", "origin", "main")
        custom = sync.git("rev-parse", "HEAD")
        with patch.object(sync, "UPSTREAM", upstream), patch.dict(os.environ, {
                "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "1"}):
            sync.prepare()
            output = Path(os.environ["GITHUB_OUTPUT"])
            self.assertEqual(output.read_text(), "changed=false\n")
            sync.git("checkout", "upstream")
            Path("new-feature").write_text("upstream survives")
            source.write_text(sync.replace_version(source.read_text(), "0.4.5"))
            self.commit("new upstream feature")
            sync.git("push", upstream, "HEAD:refs/heads/main")
            sync.git("checkout", "main")
            output.write_text("")
            sync.prepare()
            self.assertEqual(sync.git("rev-parse", "main"), custom)
            sync.git("merge-base", "--is-ancestor", custom, "HEAD")
            values = dict(line.split("=", 1) for line in output.read_text().splitlines())
            self.assertEqual(values["changed"], "true")
            self.assertTrue(values["tag"].startswith("v0.4.5-company."))
            with patch.dict(os.environ, {"COMPANY_TAG": values["tag"]}):
                sync.publish()
                sync.verify_release()
            self.assertEqual(Path("custom").read_text(), "custom survives")
            self.assertEqual(Path("new-feature").read_text(), "upstream survives")
            self.assertEqual(sync.git("--git-dir", remote, "rev-parse", "main"),
                             sync.git("rev-parse", "HEAD"))
            sync.git("merge-base", "--is-ancestor", custom, "HEAD")

    def test_prepare_real_source_conflict_restores_original(self):
        remote = str(Path(self.temp.name) / "origin.git")
        upstream = str(Path(self.temp.name) / "upstream.git")
        for target in (remote, upstream):
            sync.git("init", "--bare", target)
            sync.git("push", target, "main")
        sync.git("remote", "add", "origin", remote)
        source = Path(sync.SOURCE)
        source.write_text(sync.replace_version(source.read_text(), "0.4.4-company.1")
                          .replace('return "base"', 'return "company implementation"'))
        self.commit("custom implementation")
        original = sync.git("rev-parse", "HEAD")
        sync.git("push", "origin", "main")
        sync.git("checkout", "upstream")
        source.write_text(sync.replace_version(source.read_text(), "0.4.5")
                          .replace('return "base"', 'return "upstream implementation"'))
        self.commit("upstream implementation and version")
        sync.git("push", upstream, "HEAD:refs/heads/main")
        sync.git("checkout", "main")
        with patch.object(sync, "UPSTREAM", upstream):
            with self.assertRaises(RuntimeError):
                sync.prepare()
        self.assertEqual(sync.git("rev-parse", "HEAD"), original)
        self.assertEqual(sync.git("--git-dir", remote, "rev-parse", "main"), original)
        self.assertEqual(sync.version(source.read_text()), "0.4.4-company.1")
        self.assertIn('return "company implementation"', source.read_text())
        self.assertEqual(sync.git("status", "--porcelain"), "")
        self.assertEqual(sync.git("--git-dir", remote, "tag", "--list"), "")
        self.assertIn(sync.SOURCE, sync.report_path().read_text())


if __name__ == "__main__":
    unittest.main()
