#!/usr/bin/env python3
"""Real Python gRPC acceptance through a private relay into Firecracker vsock.

The local operator supplies the session on stdin. No credential enters argv,
stdout or the report. Assertion failures and gRPC failures exit nonzero.
"""
import json
import sys

import grpc
from google.protobuf.json_format import ParseDict
from hypercall_proto.hypercall.guest.v1 import guest_pb2 as pb
from hypercall_proto.hypercall.guest.v1 import guest_pb2_grpc as rpc


def main():
    request = json.load(sys.stdin)
    with grpc.insecure_channel(request["endpoint"]) as channel:
        client = rpc.GuestStub(channel)
        metadata = (("authorization", "Bearer " + request["credential"]),)
        info = client.GetInfo(pb.GetInfoRequest(), metadata=metadata, timeout=5)
        assert info.identity == ParseDict(request["identity"], pb.AllocationIdentity())
        assert info.protocol_version == "hypercall.guest.v1"
        data = b"\x00\xff\x80python-vsock\n\x00"
        written = client.WriteFile(pb.WriteFileRequest(path="python.bin", data=data),
                                   metadata=metadata, timeout=5)
        read = client.ReadFile(pb.ReadFileRequest(path="python.bin"), metadata=metadata, timeout=5)
        assert written.bytes_written == len(data) and read.data == data
        events = list(client.Exec(pb.ExecRequest(
            argv=["/bin/sh", "-c", "printf pyout; printf pyerr >&2; exit 9"], timeout_ms=3000),
            metadata=metadata, timeout=5))
        assert b"".join(e.stdout for e in events) == b"pyout"
        assert b"".join(e.stderr for e in events) == b"pyerr"
        results = [e.result for e in events if e.HasField("result")]
        assert len(results) == 1 and results[0].exit_code == 9
        assert events[-1].HasField("result")
        try:
            client.GetInfo(pb.GetInfoRequest(), timeout=5)
        except grpc.RpcError as error:
            assert error.code() == grpc.StatusCode.UNAUTHENTICATED
        else:
            raise AssertionError("unauthenticated Python request accepted")
    print(json.dumps({"passed": True, "transport": "Firecracker-vsock-via-private-Unix-relay",
                      "binary_bytes": len(data), "exit_code": 9, "terminal_results": 1}))


if __name__ == "__main__":
    main()
