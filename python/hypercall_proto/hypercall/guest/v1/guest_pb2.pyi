from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class AllocationIdentity(_message.Message):
    __slots__ = ("tenant_id", "project_id", "sandbox_id", "execution_id", "epoch")
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    SANDBOX_ID_FIELD_NUMBER: _ClassVar[int]
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    EPOCH_FIELD_NUMBER: _ClassVar[int]
    tenant_id: str
    project_id: str
    sandbox_id: str
    execution_id: str
    epoch: int
    def __init__(self, tenant_id: _Optional[str] = ..., project_id: _Optional[str] = ..., sandbox_id: _Optional[str] = ..., execution_id: _Optional[str] = ..., epoch: _Optional[int] = ...) -> None: ...

class GetInfoRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class GetInfoResponse(_message.Message):
    __slots__ = ("protocol_version", "identity", "max_file_bytes", "max_exec_output_bytes", "server_time_unix_nano")
    PROTOCOL_VERSION_FIELD_NUMBER: _ClassVar[int]
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    MAX_FILE_BYTES_FIELD_NUMBER: _ClassVar[int]
    MAX_EXEC_OUTPUT_BYTES_FIELD_NUMBER: _ClassVar[int]
    SERVER_TIME_UNIX_NANO_FIELD_NUMBER: _ClassVar[int]
    protocol_version: str
    identity: AllocationIdentity
    max_file_bytes: int
    max_exec_output_bytes: int
    server_time_unix_nano: int
    def __init__(self, protocol_version: _Optional[str] = ..., identity: _Optional[_Union[AllocationIdentity, _Mapping]] = ..., max_file_bytes: _Optional[int] = ..., max_exec_output_bytes: _Optional[int] = ..., server_time_unix_nano: _Optional[int] = ...) -> None: ...

class ExecRequest(_message.Message):
    __slots__ = ("argv", "cwd", "env", "timeout_ms")
    class EnvEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    ARGV_FIELD_NUMBER: _ClassVar[int]
    CWD_FIELD_NUMBER: _ClassVar[int]
    ENV_FIELD_NUMBER: _ClassVar[int]
    TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    argv: _containers.RepeatedScalarFieldContainer[str]
    cwd: str
    env: _containers.ScalarMap[str, str]
    timeout_ms: int
    def __init__(self, argv: _Optional[_Iterable[str]] = ..., cwd: _Optional[str] = ..., env: _Optional[_Mapping[str, str]] = ..., timeout_ms: _Optional[int] = ...) -> None: ...

class ExecEvent(_message.Message):
    __slots__ = ("stdout", "stderr", "result")
    STDOUT_FIELD_NUMBER: _ClassVar[int]
    STDERR_FIELD_NUMBER: _ClassVar[int]
    RESULT_FIELD_NUMBER: _ClassVar[int]
    stdout: bytes
    stderr: bytes
    result: ExecResult
    def __init__(self, stdout: _Optional[bytes] = ..., stderr: _Optional[bytes] = ..., result: _Optional[_Union[ExecResult, _Mapping]] = ...) -> None: ...

class ExecResult(_message.Message):
    __slots__ = ("exit_code", "timed_out", "output_truncated")
    EXIT_CODE_FIELD_NUMBER: _ClassVar[int]
    TIMED_OUT_FIELD_NUMBER: _ClassVar[int]
    OUTPUT_TRUNCATED_FIELD_NUMBER: _ClassVar[int]
    exit_code: int
    timed_out: bool
    output_truncated: bool
    def __init__(self, exit_code: _Optional[int] = ..., timed_out: bool = ..., output_truncated: bool = ...) -> None: ...

class ReadFileRequest(_message.Message):
    __slots__ = ("path",)
    PATH_FIELD_NUMBER: _ClassVar[int]
    path: str
    def __init__(self, path: _Optional[str] = ...) -> None: ...

class ReadFileResponse(_message.Message):
    __slots__ = ("data",)
    DATA_FIELD_NUMBER: _ClassVar[int]
    data: bytes
    def __init__(self, data: _Optional[bytes] = ...) -> None: ...

class WriteFileRequest(_message.Message):
    __slots__ = ("path", "data")
    PATH_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    path: str
    data: bytes
    def __init__(self, path: _Optional[str] = ..., data: _Optional[bytes] = ...) -> None: ...

class WriteFileResponse(_message.Message):
    __slots__ = ("bytes_written",)
    BYTES_WRITTEN_FIELD_NUMBER: _ClassVar[int]
    bytes_written: int
    def __init__(self, bytes_written: _Optional[int] = ...) -> None: ...

class StartControlledRequest(_message.Message):
    __slots__ = ("counter",)
    COUNTER_FIELD_NUMBER: _ClassVar[int]
    counter: int
    def __init__(self, counter: _Optional[int] = ...) -> None: ...

class InspectControlledRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ContinueControlledRequest(_message.Message):
    __slots__ = ("delta",)
    DELTA_FIELD_NUMBER: _ClassVar[int]
    delta: int
    def __init__(self, delta: _Optional[int] = ...) -> None: ...

class PrepareCaptureRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ReleaseCaptureRequest(_message.Message):
    __slots__ = ("host_unix_nano",)
    HOST_UNIX_NANO_FIELD_NUMBER: _ClassVar[int]
    host_unix_nano: int
    def __init__(self, host_unix_nano: _Optional[int] = ...) -> None: ...

class RebindCloneRequest(_message.Message):
    __slots__ = ("host_unix_nano",)
    HOST_UNIX_NANO_FIELD_NUMBER: _ClassVar[int]
    host_unix_nano: int
    def __init__(self, host_unix_nano: _Optional[int] = ...) -> None: ...

class ControlledState(_message.Message):
    __slots__ = ("counter", "file_data", "pid", "at_barrier", "identity", "random_probe")
    COUNTER_FIELD_NUMBER: _ClassVar[int]
    FILE_DATA_FIELD_NUMBER: _ClassVar[int]
    PID_FIELD_NUMBER: _ClassVar[int]
    AT_BARRIER_FIELD_NUMBER: _ClassVar[int]
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    RANDOM_PROBE_FIELD_NUMBER: _ClassVar[int]
    counter: int
    file_data: bytes
    pid: int
    at_barrier: bool
    identity: AllocationIdentity
    random_probe: bytes
    def __init__(self, counter: _Optional[int] = ..., file_data: _Optional[bytes] = ..., pid: _Optional[int] = ..., at_barrier: bool = ..., identity: _Optional[_Union[AllocationIdentity, _Mapping]] = ..., random_probe: _Optional[bytes] = ...) -> None: ...
