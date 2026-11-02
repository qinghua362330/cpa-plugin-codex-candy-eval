#!/usr/bin/env python3
"""Fail-closed company sync; prepare and publish are separate CI gates."""
import datetime
import json
import os
from pathlib import Path
import re
import subprocess
import sys

FORK = "qinghua362330/cpa-plugin-codex-candy-eval"
UPSTREAM = "https://github.com/haowang02/cpa-plugin-codex-candy-eval.git"
SOURCE = "internal/plugin/plugin.go"
VERSION = re.compile(r'(?m)^(\s*pluginVersion\s*=\s*")([^"\n]+)(")')
COMPANY = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?-company\.[0-9]+(?:\.[0-9]+)*")


def run(*args, check=True):
    return subprocess.run(args, text=True, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, check=check)


def git(*args):
    return run("git", *args).stdout.strip()


def version(text):
    matches = list(VERSION.finditer(text))
    if len(matches) != 1:
        raise ValueError("Expected exactly one pluginVersion declaration")
    return matches[0].group(2)


def company_version(upstream, stamp, run_id, attempt):
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?", upstream):
        raise ValueError("Unsupported upstream version; review manually")
    if "-company." in upstream:
        raise ValueError("Upstream unexpectedly contains a company version")
    for part in (stamp, run_id, attempt):
        if not re.fullmatch(r"[0-9]+", part):
            raise ValueError("Version suffix must be numeric")
    return f"{upstream}-company.{stamp}.{run_id}.{attempt}"


def replace_version(text, new):
    version(text)
    return VERSION.sub(lambda m: m.group(1) + new + m.group(3), text, count=1)


def output(name, value):
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as stream:
        stream.write(f"{name}={value}\n")


def report_path():
    return Path(os.environ["RUNNER_TEMP"]) / "company-sync-report.md"


def merge(upstream_ref):
    result = run("git", "-c", "rerere.enabled=false", "merge", "--no-ff",
                 "--no-commit", upstream_ref, check=False)
    if result.returncode:
        conflicts = git("diff", "--name-only", "--diff-filter=U")
        report_path().write_text(
            "Merge failed; main was not pushed. No automatic conflict resolution.\n\n"
            f"Upstream commit: `{git('rev-parse', upstream_ref)}`\n\n"
            f"Conflicting paths:\n```text\n{conflicts or '(see workflow log)'}\n```\n",
            encoding="utf-8")
        run("git", "merge", "--abort", check=False)
        raise RuntimeError("Upstream merge blocked; see issue report")


def prepare():
    if git("status", "--porcelain"):
        raise RuntimeError("Refusing to merge into a dirty checkout")
    git("fetch", "--no-tags", "origin", "main")
    if git("rev-parse", "HEAD") != git("rev-parse", "FETCH_HEAD"):
        raise RuntimeError("main moved after checkout; retry with a fresh run")
    git("fetch", "--no-tags", UPSTREAM, "main")
    upstream = git("rev-parse", "FETCH_HEAD")
    if run("git", "merge-base", "--is-ancestor", upstream, "HEAD", check=False).returncode == 0:
        output("changed", "false")
        return
    source_version = version(git("show", f"{upstream}:{SOURCE}"))
    original = git("rev-parse", "HEAD")
    bases = git("merge-base", "--all", original, upstream).splitlines()
    if len(bases) != 1:
        raise RuntimeError("Ambiguous merge base; review manually")
    base_version = version(git("show", f"{bases[0]}:{SOURCE}"))
    path = Path(SOURCE)
    local_source = path.read_text(encoding="utf-8")
    try:
        # Only normalize the scalar, never choose ours/theirs for a source file.
        # Work detached so no local branch moves before the validated atomic push.
        git("checkout", "--detach", original)
        if version(local_source) != base_version:
            path.write_text(replace_version(local_source, base_version), encoding="utf-8")
            git("add", "--", SOURCE)
            git("commit", "-m", "Normalize company version scalar for upstream merge")
        merge(upstream)
        stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d%H%M%S")
        new = company_version(source_version, stamp, os.environ["GITHUB_RUN_ID"],
                              os.environ["GITHUB_RUN_ATTEMPT"])
        path.write_text(replace_version(path.read_text(encoding="utf-8"), new), encoding="utf-8")
        git("add", "--", SOURCE)
        output("tag", "v" + new)
        output("changed", "true")
    except Exception:
        run("git", "merge", "--abort", check=False)
        git("checkout", "--detach", original)
        raise


def checked_tag():
    tag = os.environ["COMPANY_TAG"]
    if not COMPANY.fullmatch(tag):
        raise ValueError("Only company version tags are allowed")
    if version(Path(SOURCE).read_text(encoding="utf-8")) != tag[1:]:
        raise ValueError("Release tag does not exactly match pluginVersion")
    return tag


def publish():
    tag = checked_tag()
    git("rev-parse", "--verify", "MERGE_HEAD")
    if git("diff", "--name-only", "--diff-filter=U"):
        raise RuntimeError("Unresolved conflicts; refusing to publish")
    git("diff", "--exit-code")
    git("diff", "--cached", "--check")
    git("commit", "-m", f"Merge upstream main for {tag}")
    git("tag", "-a", tag, "-m", f"Company release {tag}")
    # Atomic non-force push: a concurrent main advance or tag collision changes neither ref.
    git("push", "--atomic", "origin", "HEAD:refs/heads/main", f"refs/tags/{tag}")


def verify_release():
    tag = checked_tag()
    if git("rev-parse", "HEAD") != git("rev-parse", f"refs/tags/{tag}^{{commit}}"):
        raise RuntimeError("Checkout is not the requested tag")
    git("merge-base", "--is-ancestor", "HEAD", "refs/remotes/origin/main")


def report():
    title = "Company upstream sync blocked"
    url = (f"{os.environ['GITHUB_SERVER_URL']}/{FORK}/actions/runs/"
           f"{os.environ['GITHUB_RUN_ID']}")
    details = report_path().read_text(encoding="utf-8") if report_path().exists() else (
        "Sync validation or atomic push failed. Inspect the run before retrying.\n")
    body = f"{details}\nRun: {url}\n\nResolve manually on main; never force-reset to upstream."
    issues = json.loads(run("gh", "issue", "list", "--repo", FORK, "--state", "open",
                            "--author", "github-actions[bot]", "--search", f'"{title}" in:title',
                            "--json", "number,title", "--limit", "100").stdout)
    existing = next((item for item in issues if item["title"] == title), None)
    if existing:
        run("gh", "issue", "edit", str(existing["number"]), "--repo", FORK, "--body", body)
    else:
        run("gh", "issue", "create", "--repo", FORK, "--title", title, "--body", body)


def main():
    if os.environ.get("GITHUB_REPOSITORY") != FORK:
        raise RuntimeError("This automation may only run in the owned fork")
    commands = {"prepare": prepare, "publish": publish,
                "verify-release": verify_release, "report": report}
    commands[sys.argv[1]]()


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as error:
        print(error.stderr, file=sys.stderr)
        sys.exit(error.returncode)
