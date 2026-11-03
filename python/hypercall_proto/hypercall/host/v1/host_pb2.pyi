from hypercall_proto.hypercall.guest.v1 import guest_pb2 as _guest_pb2
from hypercall_proto.hypercall.v1 import control_pb2 as _control_pb2
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class Assignment(_message.Message):
    __slots__ = ("tenant_id", "project_id", "branch_id", "execution_id", "epoch", "lease_until_unix_nano", "resolved_run_json", "sandbox_id")
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    EPOCH_FIELD_NUMBER: _ClassVar[int]
    LEASE_UNTIL_UNIX_NANO_FIELD_NUMBER: _ClassVar[int]
    RESOLVED_RUN_JSON_FIELD_NUMBER: _ClassVar[int]
    SANDBOX_ID_FIELD_NUMBER: _ClassVar[int]
    tenant_id: str
    project_id: str
    branch_id: str
    execution_id: str
    epoch: int
    lease_until_unix_nano: int
    resolved_run_json: bytes
    sandbox_id: str
    def __init__(self, tenant_id: _Optional[str] = ..., project_id: _Optional[str] = ..., branch_id: _Optional[str] = ..., execution_id: _Optional[str] = ..., epoch: _Optional[int] = ..., lease_until_unix_nano: _Optional[int] = ..., resolved_run_json: _Optional[bytes] = ..., sandbox_id: _Optional[str] = ...) -> None: ...

class Observation(_message.Message):
    __slots__ = ("state", "checkpoint")
    STATE_FIELD_NUMBER: _ClassVar[int]
    CHECKPOINT_FIELD_NUMBER: _ClassVar[int]
    state: str
    checkpoint: _control_pb2.CheckpointSpec
    def __init__(self, state: _Optional[str] = ..., checkpoint: _Optional[_Union[_control_pb2.CheckpointSpec, _Mapping]] = ...) -> None: ...

class ResumeRequest(_message.Message):
    __slots__ = ("assignment", "checkpoint")
    ASSIGNMENT_FIELD_NUMBER: _ClassVar[int]
    CHECKPOINT_FIELD_NUMBER: _ClassVar[int]
    assignment: Assignment
    checkpoint: _control_pb2.CheckpointSpec
    def __init__(self, assignment: _Optional[_Union[Assignment, _Mapping]] = ..., checkpoint: _Optional[_Union[_control_pb2.CheckpointSpec, _Mapping]] = ...) -> None: ...

class ExecRequest(_message.Message):
    __slots__ = ("assignment", "command")
    ASSIGNMENT_FIELD_NUMBER: _ClassVar[int]
    COMMAND_FIELD_NUMBER: _ClassVar[int]
    assignment: Assignment
    command: _guest_pb2.ExecRequest
    def __init__(self, assignment: _Optional[_Union[Assignment, _Mapping]] = ..., command: _Optional[_Union[_guest_pb2.ExecRequest, _Mapping]] = ...) -> None: ...

class ReadFileRequest(_message.Message):
    __slots__ = ("assignment", "file")
    ASSIGNMENT_FIELD_NUMBER: _ClassVar[int]
    FILE_FIELD_NUMBER: _ClassVar[int]
    assignment: Assignment
    file: _guest_pb2.ReadFileRequest
    def __init__(self, assignment: _Optional[_Union[Assignment, _Mapping]] = ..., file: _Optional[_Union[_guest_pb2.ReadFileRequest, _Mapping]] = ...) -> None: ...

class WriteFileRequest(_message.Message):
    __slots__ = ("assignment", "file")
    ASSIGNMENT_FIELD_NUMBER: _ClassVar[int]
    FILE_FIELD_NUMBER: _ClassVar[int]
    assignment: Assignment
    file: _guest_pb2.WriteFileRequest
    def __init__(self, assignment: _Optional[_Union[Assignment, _Mapping]] = ..., file: _Optional[_Union[_guest_pb2.WriteFileRequest, _Mapping]] = ...) -> None: ...
