#!/usr/bin/env python3
"""Real Docker mirror pull with a disposable daemon, no host daemon changes.
Requires Docker (privileged containers), openssl and Python 3. Downloads docker:29-dind
and alpine:latest. The isolated daemon has no external network, preventing Hub fallback.
Usage: python3 scripts/mirror-smoke.py [proxy-image]
"""
import io
import json
import pathlib
import subprocess
import sys
import tarfile
import tempfile
import time
import uuid

image = sys.argv[1] if len(sys.argv) > 1 else "oci-pull-proxy:verification"
dind_image = "docker:29-dind"
prefix = "mirror-smoke-" + uuid.uuid4().hex[:10]
proxy, daemon, network = prefix + "-proxy", prefix + "-docker", prefix + "-net"
containers = []
token = ""


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True, stderr=subprocess.STDOUT).strip()


def wait_for(args):
    deadline = time.monotonic() + 60
    while True:
        try:
            return docker(*args)
        except subprocess.CalledProcessError:
            if time.monotonic() > deadline:
                raise
            time.sleep(0.5)


def read_token():
    archive = subprocess.check_output(["docker", "cp", proxy + ":/config/.env", "-"])
    with tarfile.open(fileobj=io.BytesIO(archive)) as files:
        text = files.extractfile(files.getmembers()[0]).read().decode()
        assert text.startswith("# keep this comment\n")
        return next((line.split("=", 1)[1] for line in text.splitlines() if line.startswith("TOKEN=")), "")


try:
    docker("pull", dind_image)
    docker("network", "create", "--internal", network)
    with tempfile.TemporaryDirectory(prefix=prefix) as tmp:
        root = pathlib.Path(tmp)
        root.chmod(0o755)
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                        "-subj", "/CN=mirror", "-addext", "subjectAltName=DNS:mirror",
                        "-keyout", str(root / "key.pem"), "-out", str(root / "cert.pem")],
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        (root / "key.pem").chmod(0o644)  # disposable test key only
        token_file = root / ".env"
        token_file.write_text("# keep this comment\nTOKEN=\n")
        docker("run", "--rm", "--entrypoint", "sh", "-v", f"{root}:/fixture", dind_image,
               "-c", "chown 65532:65532 /fixture/.env && chmod 600 /fixture/.env")
        for iteration in range(2):
            docker("create", "--name", proxy, "--read-only", "--network", "bridge",
                   "-v", f"{root}:/fixture:ro", "--mount", f"type=bind,src={token_file},dst=/config/.env",
                   "--tmpfs", "/data/cache:uid=65532,gid=65532,mode=0700",
                   "-e", "PROXY_USERNAME=test", "-e", "PROXY_PASSWORD=smoke-password",
                   "-e", "TLS_MODE=manual", "-e", "TLS_CERT=/fixture/cert.pem", "-e", "TLS_KEY=/fixture/key.pem", image)
            containers.append(proxy)
            docker("network", "connect", "--alias", "mirror", network, proxy)
            docker("start", proxy)
            deadline = time.monotonic() + 20
            while len(read_token()) != 43:
                if time.monotonic() > deadline:
                    raise RuntimeError("proxy did not create persistent token")
                time.sleep(0.2)
            token = read_token()
            if iteration == 0:
                original_token = token
                docker("stop", proxy)
                docker("rm", proxy)
                containers.remove(proxy)
            else:
                assert token == original_token, "token changed on recreation"
        docker("run", "-d", "--privileged", "--name", daemon, "--network", network,
               "-e", "DOCKER_TLS_CERTDIR=", "--mount",
               f"type=bind,src={root / 'cert.pem'},dst=/usr/local/share/ca-certificates/mirror.crt,readonly",
               "--entrypoint", "sh", dind_image, "-c",
               'update-ca-certificates >/dev/null && exec dockerd-entrypoint.sh "$@"',
               "sh", "--registry-mirror=https://mirror:8443/" + token)
        containers.append(daemon)
        wait_for(["exec", daemon, "docker", "info"])
        # No published ports and no egress from this daemon: success must use the mirror.
        result = docker("exec", daemon, "docker", "pull", "alpine:latest")
        assert "sha256:" in result
        print(result)
        assert token not in docker("logs", proxy), "token leaked to service logs"
        print("PASS: TOKEN in .env survived recreation; real Docker pulled via HTTPS token mirror without Hub fallback")
except subprocess.CalledProcessError as exc:
    # Do not print daemon command arguments or logs: they contain the disposable token.
    print("Docker integration command failed (exit %s)" % exc.returncode, file=sys.stderr)
    detail = exc.output or ""
    if token:
        detail = detail.replace(token, "[redacted]")
    print(detail, file=sys.stderr)
    for name in containers:
        logs = docker("logs", "--tail", "15", name)
        if token:
            logs = logs.replace(token, "[redacted]")
        print(name + ": " + logs, file=sys.stderr)
    raise SystemExit(1)
finally:
    for name in reversed(containers):
        docker("rm", "-f", "-v", name)
    try:
        docker("network", "rm", network)
    except subprocess.CalledProcessError:
        pass
