from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class GetSessionRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class HarnessSession(_message.Message):
    __slots__ = ("tenant_id", "project_id", "branch_id", "sandbox_id", "execution_id", "epoch", "call_namespace", "route_digest", "model_digest", "protocol_version", "session_hash", "parent_checkpoint_id", "inherited_barrier")
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    SANDBOX_ID_FIELD_NUMBER: _ClassVar[int]
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    EPOCH_FIELD_NUMBER: _ClassVar[int]
    CALL_NAMESPACE_FIELD_NUMBER: _ClassVar[int]
    ROUTE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    MODEL_DIGEST_FIELD_NUMBER: _ClassVar[int]
    PROTOCOL_VERSION_FIELD_NUMBER: _ClassVar[int]
    SESSION_HASH_FIELD_NUMBER: _ClassVar[int]
    PARENT_CHECKPOINT_ID_FIELD_NUMBER: _ClassVar[int]
    INHERITED_BARRIER_FIELD_NUMBER: _ClassVar[int]
    tenant_id: str
    project_id: str
    branch_id: str
    sandbox_id: str
    execution_id: str
    epoch: int
    call_namespace: str
    route_digest: str
    model_digest: str
    protocol_version: str
    session_hash: str
    parent_checkpoint_id: str
    inherited_barrier: SemanticBarrier
    def __init__(self, tenant_id: _Optional[str] = ..., project_id: _Optional[str] = ..., branch_id: _Optional[str] = ..., sandbox_id: _Optional[str] = ..., execution_id: _Optional[str] = ..., epoch: _Optional[int] = ..., call_namespace: _Optional[str] = ..., route_digest: _Optional[str] = ..., model_digest: _Optional[str] = ..., protocol_version: _Optional[str] = ..., session_hash: _Optional[str] = ..., parent_checkpoint_id: _Optional[str] = ..., inherited_barrier: _Optional[_Union[SemanticBarrier, _Mapping]] = ...) -> None: ...

class SemanticBarrier(_message.Message):
    __slots__ = ("protocol_version", "step", "event_sequence", "state_digest", "source_session_hash", "source_branch_id", "source_execution_id", "source_epoch")
    PROTOCOL_VERSION_FIELD_NUMBER: _ClassVar[int]
    STEP_FIELD_NUMBER: _ClassVar[int]
    EVENT_SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    STATE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    SOURCE_SESSION_HASH_FIELD_NUMBER: _ClassVar[int]
    SOURCE_BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    SOURCE_EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    SOURCE_EPOCH_FIELD_NUMBER: _ClassVar[int]
    protocol_version: str
    step: int
    event_sequence: int
    state_digest: str
    source_session_hash: str
    source_branch_id: str
    source_execution_id: str
    source_epoch: int
    def __init__(self, protocol_version: _Optional[str] = ..., step: _Optional[int] = ..., event_sequence: _Optional[int] = ..., state_digest: _Optional[str] = ..., source_session_hash: _Optional[str] = ..., source_branch_id: _Optional[str] = ..., source_execution_id: _Optional[str] = ..., source_epoch: _Optional[int] = ...) -> None: ...

class AgentRequest(_message.Message):
    __slots__ = ("id", "kind", "step", "position", "name", "payload", "payload_digest", "session_hash")
    ID_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    STEP_FIELD_NUMBER: _ClassVar[int]
    POSITION_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    PAYLOAD_FIELD_NUMBER: _ClassVar[int]
    PAYLOAD_DIGEST_FIELD_NUMBER: _ClassVar[int]
    SESSION_HASH_FIELD_NUMBER: _ClassVar[int]
    id: str
    kind: str
    step: int
    position: int
    name: str
    payload: bytes
    payload_digest: str
    session_hash: str
    def __init__(self, id: _Optional[str] = ..., kind: _Optional[str] = ..., step: _Optional[int] = ..., position: _Optional[int] = ..., name: _Optional[str] = ..., payload: _Optional[bytes] = ..., payload_digest: _Optional[str] = ..., session_hash: _Optional[str] = ...) -> None: ...

class AgentResult(_message.Message):
    __slots__ = ("id", "state", "payload", "event_sequence", "error_code", "replayed")
    ID_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    PAYLOAD_FIELD_NUMBER: _ClassVar[int]
    EVENT_SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    ERROR_CODE_FIELD_NUMBER: _ClassVar[int]
    REPLAYED_FIELD_NUMBER: _ClassVar[int]
    id: str
    state: str
    payload: bytes
    event_sequence: int
    error_code: str
    replayed: bool
    def __init__(self, id: _Optional[str] = ..., state: _Optional[str] = ..., payload: _Optional[bytes] = ..., event_sequence: _Optional[int] = ..., error_code: _Optional[str] = ..., replayed: bool = ...) -> None: ...

class AgentBarrier(_message.Message):
    __slots__ = ("step", "event_sequence", "state_digest", "protocol_version", "session_hash")
    STEP_FIELD_NUMBER: _ClassVar[int]
    EVENT_SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    STATE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    PROTOCOL_VERSION_FIELD_NUMBER: _ClassVar[int]
    SESSION_HASH_FIELD_NUMBER: _ClassVar[int]
    step: int
    event_sequence: int
    state_digest: str
    protocol_version: str
    session_hash: str
    def __init__(self, step: _Optional[int] = ..., event_sequence: _Optional[int] = ..., state_digest: _Optional[str] = ..., protocol_version: _Optional[str] = ..., session_hash: _Optional[str] = ...) -> None: ...

class AgentPoll(_message.Message):
    __slots__ = ("request", "session_ack", "barrier")
    REQUEST_FIELD_NUMBER: _ClassVar[int]
    SESSION_ACK_FIELD_NUMBER: _ClassVar[int]
    BARRIER_FIELD_NUMBER: _ClassVar[int]
    request: AgentRequest
    session_ack: str
    barrier: AgentBarrier
    def __init__(self, request: _Optional[_Union[AgentRequest, _Mapping]] = ..., session_ack: _Optional[str] = ..., barrier: _Optional[_Union[AgentBarrier, _Mapping]] = ...) -> None: ...

class AgentDelivery(_message.Message):
    __slots__ = ("session_hash", "request_hash", "result", "session_ack", "sealed_barrier")
    SESSION_HASH_FIELD_NUMBER: _ClassVar[int]
    REQUEST_HASH_FIELD_NUMBER: _ClassVar[int]
    RESULT_FIELD_NUMBER: _ClassVar[int]
    SESSION_ACK_FIELD_NUMBER: _ClassVar[int]
    SEALED_BARRIER_FIELD_NUMBER: _ClassVar[int]
    session_hash: str
    request_hash: str
    result: AgentResult
    session_ack: str
    sealed_barrier: AgentBarrier
    def __init__(self, session_hash: _Optional[str] = ..., request_hash: _Optional[str] = ..., result: _Optional[_Union[AgentResult, _Mapping]] = ..., session_ack: _Optional[str] = ..., sealed_barrier: _Optional[_Union[AgentBarrier, _Mapping]] = ...) -> None: ...

class SessionAckRequest(_message.Message):
    __slots__ = ("session_hash",)
    SESSION_HASH_FIELD_NUMBER: _ClassVar[int]
    session_hash: str
    def __init__(self, session_hash: _Optional[str] = ...) -> None: ...

class SessionAck(_message.Message):
    __slots__ = ("session_hash",)
    SESSION_HASH_FIELD_NUMBER: _ClassVar[int]
    session_hash: str
    def __init__(self, session_hash: _Optional[str] = ...) -> None: ...
