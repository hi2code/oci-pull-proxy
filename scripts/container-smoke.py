#!/usr/bin/env python3
"""Verify an already-built image with disposable volumes and a local test cert.
Usage: python3 scripts/container-smoke.py [image]
Requires Docker, openssl and Python 3. Does not contact any ACME service.
"""
import base64
import http.client
import json
import pathlib
import ssl
import socket
import subprocess
import sys
import tempfile
import time
import uuid

image = sys.argv[1] if len(sys.argv) > 1 else "oci-pull-proxy:verification"
prefix = "oci-smoke-" + uuid.uuid4().hex[:12]
cache, acme, name = prefix + "-cache", prefix + "-acme", prefix + "-container"
env_directory = tempfile.TemporaryDirectory(prefix="oci-smoke-env-")
env_file = pathlib.Path(env_directory.name) / ".env"

def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()

def create(command=None, manual=False):
    args = ["create", "--name", name, "--read-only", "--cap-drop=ALL",
            "--security-opt=no-new-privileges", "--mount", f"source={cache},target=/data/cache",
            "--mount", f"source={acme},target=/data/acme",
            "--mount", f"type=bind,src={env_file},dst=/config/.env", "--network", "bridge",
            "--env-file", str(env_file), "-e", "ACME_ACCEPT_TOS=true",
            "-e", "ACME_DIRECTORY_URL=https://127.0.0.1:1/directory"]
    if command:
        # This preflight must not pre-generate the token under test.
        args += ["-e", "TOKEN=preflight-only"]
    if manual:
        args += ["-e", "TLS_MODE=manual", "-e", "TLS_CERT=/data/acme/cert.pem",
                 "-e", "TLS_KEY=/data/acme/key.pem", "-p", "127.0.0.1::8443", "-p", "127.0.0.1::8080"]
    args += [image]
    if command:
        args += [command]
    docker(*args)

def read_token():
    text = env_file.read_text()
    assert text.startswith("# preserved comment\nPROXY_USERNAME=smoke\n")
    return next(line.split("=", 1)[1] for line in text.splitlines() if line.startswith("TOKEN="))

class LocalTLSConnection(http.client.HTTPSConnection):
    def connect(self):
        # Connect locally while sending the configured domain in TLS SNI.
        raw = socket.create_connection((self.host, self.port), self.timeout)
        try:
            self.sock = self._context.wrap_socket(raw, server_hostname="images.example.com")
        except Exception:
            raw.close()
            raise

def request(port, path, secure=True, auth=False):
    conn = (LocalTLSConnection("127.0.0.1", port, context=ssl._create_unverified_context(), timeout=3)
            if secure else http.client.HTTPConnection("127.0.0.1", port, timeout=3))
    headers = {}
    if auth:
        headers["Authorization"] = "Basic " + base64.b64encode(b"smoke:smoke-only-disposable-secret").decode()
    try:
        conn.request("GET", path, headers=headers)
        response = conn.getresponse()
        return response.status, response.read(), response.getheader("Location")
    finally:
        conn.close()

try:
    env_file.write_text("# preserved comment\nPROXY_USERNAME=smoke\n"
                        "PROXY_PASSWORD=smoke-only-disposable-secret\n"
                        "PROXY_DOMAIN=images.example.com\nTOKEN=\n")
    # Disposable fixture only: let the non-root container write the host file.
    env_file.chmod(0o666)
    create("check")
    assert docker("inspect", "--format", "{{.Config.User}}", name) == "65532:65532"
    print(docker("start", "-a", name))
    assert docker("inspect", "--format", "{{.State.ExitCode}}", name) == "0"
    assert read_token() == "", "preflight generated TOKEN before normal startup"
    with tempfile.TemporaryDirectory(prefix="oci-smoke-cert-") as tmp:
        tmp = pathlib.Path(tmp)
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                        "-subj", "/CN=images.example.com", "-addext", "subjectAltName=DNS:images.example.com",
                        "-keyout", str(tmp / "key.pem"), "-out", str(tmp / "cert.pem")],
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        # Publicly readable only for this disposable test key in an isolated test volume.
        (tmp / "key.pem").chmod(0o644)
        docker("cp", str(tmp / "cert.pem"), name + ":/data/acme/cert.pem")
        docker("cp", str(tmp / "key.pem"), name + ":/data/acme/key.pem")
    docker("rm", name)
    for iteration in range(2):
        create(manual=True)
        docker("start", name)
        ports = json.loads(docker("inspect", "--format", "{{json .NetworkSettings.Ports}}", name))
        secure = int(ports["8443/tcp"][0]["HostPort"])
        plain = int(ports["8080/tcp"][0]["HostPort"])
        deadline = time.monotonic() + 10
        while True:
            try:
                if request(secure, "/readyz")[0] == 200:
                    break
            except (OSError, http.client.HTTPException):
                pass
            if time.monotonic() >= deadline:
                raise RuntimeError("container not ready: " + docker("logs", "--tail", "10", name))
            time.sleep(0.05)
        if iteration == 0:
            original_token = read_token()
            assert len(original_token) == 43, "normal startup did not write TOKEN to the host .env"
        else:
            assert read_token() == original_token, "TOKEN changed on container recreation"
        assert request(secure, "/m/" + original_token + "/v2/")[0] == 200
        assert request(secure, "/m/wrong-token/v2/")[0] == 401
        assert original_token not in docker("logs", name), "TOKEN leaked to logs"
        assert request(secure, "/healthz")[:2] == (200, b"ok\n")
        assert request(secure, "/v2/")[0] == 401
        assert request(secure, "/v2/", auth=True)[0] == 200
        result = request(plain, "/v2/", secure=False, auth=True)
        assert result[0] == 308 and result[2] == "https://images.example.com/v2/"
        docker("stop", "-t", "30", name)
        assert docker("inspect", "--format", "{{.State.ExitCode}}", name) == "0"
        docker("rm", name)
        if iteration == 0:
            docker("volume", "rm", cache)
    print("PASS: non-root, writable fresh volumes, HTTPS/auth, HTTP redirect, graceful stop, certificates survive cache-volume replacement, normal startup writes host .env TOKEN, which survives recreation and authenticates mirror")
finally:
    subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for volume in (cache, acme):
        subprocess.run(["docker", "volume", "rm", volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    env_directory.cleanup()
