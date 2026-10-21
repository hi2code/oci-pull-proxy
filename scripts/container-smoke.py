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

def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()

def create(command=None, manual=False):
    args = ["create", "--name", name, "--read-only", "--cap-drop=ALL",
            "--security-opt=no-new-privileges", "--mount", f"source={cache},target=/data/cache",
            "--mount", f"source={acme},target=/data/acme", "--network", "bridge",
            "-e", "PROXY_DOMAIN=images.example.com", "-e", "PROXY_USERNAME=smoke",
            "-e", "PROXY_PASSWORD=smoke-only-disposable-secret", "-e", "ACME_ACCEPT_TOS=true",
            "-e", "ACME_DIRECTORY_URL=https://127.0.0.1:1/directory"]
    if manual:
        args += ["-e", "TLS_MODE=manual", "-e", "TLS_CERT=/data/acme/cert.pem",
                 "-e", "TLS_KEY=/data/acme/key.pem", "-p", "127.0.0.1::8443", "-p", "127.0.0.1::8080"]
    args += [image]
    if command:
        args += [command]
    docker(*args)

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
    create("check")
    assert docker("inspect", "--format", "{{.Config.User}}", name) == "65532:65532"
    print(docker("start", "-a", name))
    assert docker("inspect", "--format", "{{.State.ExitCode}}", name) == "0"
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
    print("PASS: non-root, writable fresh volumes, HTTPS/auth, HTTP redirect, graceful stop, certificates survive cache-volume replacement")
finally:
    subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for volume in (cache, acme):
        subprocess.run(["docker", "volume", "rm", volume], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
