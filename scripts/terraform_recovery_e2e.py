#!/usr/bin/env python3
"""Pinned Terraform + disposable local Zot persistence and recovery experiment.

Never print Terraform output: state and diagnostics can contain sensitive data.
Only synthetic resources are used. Failed runs retain private local evidence;
CI must not upload that directory or enable TF_LOG.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.request
import uuid


VERSION = "1.17.0-alpha20260827"
STATE_TAG = "state-" + hashlib.sha256(b"default").hexdigest()


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--terraform", default="terraform")
    parser.add_argument("--provider-dir", required=True)
    parser.add_argument("--registry", default="localhost:5001")
    parser.add_argument("--zot-binary", help="Start a disposable native Zot v2.1.0 instead")
    args = parser.parse_args()
    os.umask(0o077)
    root = Path(tempfile.mkdtemp(prefix="oras-recovery-e2e-"))
    zot = None
    succeeded = False
    try:
        registry = args.registry
        if args.zot_binary:
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0))
                port = listener.getsockname()[1]
            registry = f"127.0.0.1:{port}"
            config = root / "zot.json"
            config.write_text(json.dumps({
                "storage": {"rootDirectory": str(root / "registry")},
                "http": {"address": "127.0.0.1", "port": str(port)},
                "log": {"level": "error"},
            }))
            zot = subprocess.Popen([args.zot_binary, "serve", str(config)],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        require(registry.split(":")[0] in ("localhost", "127.0.0.1"),
                "Only a disposable loopback registry is allowed")
        # Bypass ambient proxy/credentials for the local registry.
        http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        for _ in range(100):
            try:
                with http.open(f"http://{registry}/v2/", timeout=1):
                    break
            except OSError:
                time.sleep(0.1)
        else:
            raise RuntimeError("Local Zot did not become ready")

        env = {k: v for k, v in os.environ.items()
               if not k.startswith(("TF_", "ORAS_", "GHCR_", "GITHUB_", "DOCKER_"))}
        # Isolate credential discovery and prevent logging state/credentials.
        env.update(HOME=str(root), XDG_CONFIG_HOME=str(root / "config"),
                   TF_ENABLE_PLUGGABLE_STATE_STORAGE="1", TF_IN_AUTOMATION="1",
                   TF_INPUT="0", CHECKPOINT_DISABLE="1", NO_PROXY="localhost,127.0.0.1")
        rc = root / "terraform.rc"
        rc.write_text('provider_installation {\n dev_overrides {\n'
                      f'  "vmvarela/oras" = {json.dumps(str(Path(args.provider_dir).resolve()))}\n'
                      ' }\n direct {}\n}\n')
        env["TF_CLI_CONFIG_FILE"] = str(rc)
        sequence = 0

        def tf(directory, *arguments, expected=0):
            nonlocal sequence
            sequence += 1
            result = subprocess.run([args.terraform, *arguments], cwd=directory,
                                    env=env, capture_output=True, timeout=120)
            (root / f"{sequence:02d}.stdout").write_bytes(result.stdout)
            (root / f"{sequence:02d}.stderr").write_bytes(result.stderr)
            require(result.returncode == expected,
                    f"Step {sequence} ({arguments[0]}): expected exit {expected}, got {result.returncode}")
            return result.stdout, result.stderr

        version, _ = tf(root, "version", "-json")
        require(json.loads(version)["terraform_version"] == VERSION, "Unexpected Terraform version")
        print(f"Terraform {VERSION}; Zot v2.1.0 fixture; synthetic resources only", flush=True)

        def fixture(name, ttl):
            directory = root / name
            directory.mkdir()
            repository = "recovery-e2e/" + name + "-" + uuid.uuid4().hex
            (directory / "main.tf").write_text('''
terraform {
  required_providers { oras = { source = "vmvarela/oras" } }
  state_store "oras_oci" {
    provider "oras" { plain_http = true }
    url = "oci://REGISTRY/REPOSITORY"
    lock_ttl = "TTL"
  }
}
variable "value" { type = string }
variable "delay" { default = 0 }
resource "terraform_data" "effect" {
  input = var.value
  triggers_replace = [var.value]
  provisioner "local-exec" {
    command = "printf '%s' '${var.value}' > effect.txt; sleep ${var.delay}"
  }
}
output "value" { value = terraform_data.effect.output }
'''.replace("REGISTRY", registry).replace("REPOSITORY", repository).replace("TTL", ttl))
            tf(directory, "init", "-input=false", "-no-color")
            return directory, repository

        def apply(directory, value, delay=0, expected=0):
            return tf(directory, "apply", "-auto-approve", "-input=false", "-no-color",
                      f"-var=value={value}", f"-var=delay={delay}", expected=expected)

        def pull(directory):
            raw, _ = tf(directory, "state", "pull")
            return json.loads(raw)

        def check_state(state, value):
            require(state["outputs"]["value"]["value"] == value, "Wrong persisted output")
            resources = [r for r in state["resources"] if r["type"] == "terraform_data"]
            require(len(resources) == 1, "Wrong persisted resource count")
            attrs = resources[0]["instances"][0]["attributes"]
            require(attrs["input"]["value"] == value, "Wrong persisted resource input")
            require(bool(attrs["id"]), "Persisted resource ID is missing")

        def manifest(repository):
            req = urllib.request.Request(f"http://{registry}/v2/{repository}/manifests/{STATE_TAG}",
                                         headers={"Accept": "application/vnd.oci.image.manifest.v1+json"})
            with http.open(req, timeout=5) as response:
                return hashlib.sha256(response.read()).hexdigest()

        normal, _ = fixture("persistence", "0")
        apply(normal, "first")
        first = pull(normal)  # Every tf() invocation creates a fresh process/provider.
        check_state(first, "first")
        apply(normal, "second")
        second = pull(normal)
        check_state(second, "second")
        require(first["lineage"] == second["lineage"], "Lineage changed across update")
        require(second["serial"] > first["serial"], "Serial did not advance")
        print("PASS apply -> fresh-process pull -> update -> fresh-process pull", flush=True)

        expiry, repository = fixture("expiry", "5s")
        apply(expiry, "before")
        before = pull(expiry)
        before_manifest = manifest(repository)
        stdout, stderr = apply(expiry, "after", delay=8, expected=1)
        diagnostics = stdout + stderr
        require(b"State lock no longer held" in diagnostics, "Missing expired-lock diagnostic")
        require(b"errored.tfstate" in diagnostics, "Missing recovery artifact diagnostic")
        require((expiry / "effect.txt").read_text() == "after", "External effect did not occur")
        recovery = expiry / "errored.tfstate"
        require(recovery.is_file(), "Terraform did not save a recovery artifact")
        recovered = json.loads(recovery.read_bytes())
        check_state(recovered, "after")
        require(recovered["lineage"] == before["lineage"], "Recovery lineage differs")
        require(recovered["serial"] > before["serial"], "Recovery serial did not advance")
        require(manifest(repository) == before_manifest, "Rejected write changed the state manifest")
        require(pull(expiry) == before, "Rejected write changed remote state")
        print("PASS expired lease: external effect occurred, write refused, remote unchanged, recovery saved", flush=True)

        # No competing writers in this isolated repository. Real incidents must
        # stop writers and reconcile lineage/serial before attempting this command.
        tf(expiry, "state", "push", "errored.tfstate")
        restored = pull(expiry)
        check_state(restored, "after")
        require(restored["lineage"] == recovered["lineage"], "Restored lineage differs")
        require(restored["serial"] >= recovered["serial"], "Restored serial regressed")
        tf(expiry, "plan", "-input=false", "-no-color", "-detailed-exitcode",
           "-var=value=after", "-var=delay=0")
        print("PASS recovery: state push without force/unlocked writes -> reread -> no-change plan", flush=True)
        succeeded = True
    finally:
        if zot:
            zot.terminate()
            try:
                zot.wait(timeout=5)
            except subprocess.TimeoutExpired:
                zot.kill()
                zot.wait()
        if succeeded:
            shutil.rmtree(root)  # Only the exact disposable directory created above.
        else:
            print(f"FAIL: private synthetic evidence retained at {root}; do not upload logs/state", flush=True)


if __name__ == "__main__":
    main()
