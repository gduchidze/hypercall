"""Trusted byte relay for Docker Desktop and remote Linux Docker daemons.

The gRPC endpoint stays on a private container Unix socket; no host port, Docker
socket, or host IPC namespace is made available to the worker.
"""
import os
import selectors
import socket
import sys
import time


def main():
    sock = socket.socket(socket.AF_UNIX)
    end = time.monotonic() + 15
    while True:
        try:
            sock.connect("/tmp/scorer.sock")
            break
        except (FileNotFoundError, ConnectionRefusedError):
            if time.monotonic() >= end:
                raise
            time.sleep(0.02)
    selector = selectors.DefaultSelector()
    selector.register(sys.stdin.buffer, selectors.EVENT_READ)
    selector.register(sock, selectors.EVENT_READ)
    while True:
        for key, _ in selector.select():
            if key.fileobj is sock:
                data = sock.recv(65536)
                if not data:
                    return
                sys.stdout.buffer.write(data)
                sys.stdout.buffer.flush()
            else:
                data = os.read(sys.stdin.fileno(), 65536)
                if not data:
                    return
                sock.sendall(data)


if __name__ == "__main__":
    main()
