#!/usr/bin/env python3
"""Serves an HTTP forward proxy (CONNECT and absolute-URI requests) on a unix socket: the build VM's silkd egress door.
usage: cut-proxy.py <vsock_socket>_2049"""

import os
import socket
import sys
import threading
import urllib.parse

HOP = ("connection", "proxy-connection", "keep-alive", "proxy-authorization")


def pipe(src, dst):
    try:
        while data := src.recv(1 << 16):
            dst.sendall(data)
        dst.shutdown(socket.SHUT_WR)
    except OSError:
        pass


def handle(client):
    with client:
        head = b""
        while b"\r\n\r\n" not in head:
            data = client.recv(1 << 16)
            if not data:
                return
            head += data
        head, rest = head.split(b"\r\n\r\n", 1)
        lines = head.decode("latin-1").split("\r\n")
        method, target, version = lines[0].split(" ", 2)
        try:
            if method == "CONNECT":
                host, port = target.rsplit(":", 1)
                upstream = socket.create_connection((host, int(port)), 30)
                client.sendall(b"HTTP/1.1 200 Connection established\r\n\r\n")
            else:
                url = urllib.parse.urlsplit(target)
                upstream = socket.create_connection((url.hostname, url.port or 80), 30)
                path = (url.path or "/") + ("?" + url.query if url.query else "")
                headers = [h for h in lines[1:] if h.split(":", 1)[0].strip().lower() not in HOP]
                upstream.sendall("\r\n".join([f"{method} {path} {version}", *headers, "Connection: close", "", ""]).encode("latin-1"))
        except (OSError, ValueError) as e:
            print(f"cut-proxy: {method} {target}: {e}", file=sys.stderr, flush=True)
            client.sendall(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
            return
        with upstream:
            if rest:
                upstream.sendall(rest)
            threading.Thread(target=pipe, args=(client, upstream), daemon=True).start()
            pipe(upstream, client)


def main():
    path = sys.argv[1] if len(sys.argv) == 2 else sys.exit(__doc__)
    if os.path.exists(path):
        os.unlink(path)
    server = socket.socket(socket.AF_UNIX)
    server.bind(path)
    server.listen(64)
    while True:
        conn, _ = server.accept()
        threading.Thread(target=handle, args=(conn,), daemon=True).start()


if __name__ == "__main__":
    main()
