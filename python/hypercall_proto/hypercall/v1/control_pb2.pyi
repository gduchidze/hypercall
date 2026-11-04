import datetime

from google.protobuf import struct_pb2 as _struct_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from hypercall_proto.hypercall.guest.v1 import guest_pb2 as _guest_pb2
from hypercall_proto.hypercall.harness.v1 import harness_pb2 as _harness_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class Metadata(_message.Message):
    __slots__ = ("id", "tenant_id", "project_id", "name", "schema_version", "revision", "digest", "created_at")
    ID_FIELD_NUMBER: _ClassVar[int]
    TENANT_ID_FIELD_NUMBER: _ClassVar[int]
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_VERSION_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    DIGEST_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    id: str
    tenant_id: str
    project_id: str
    name: str
    schema_version: str
    revision: int
    digest: str
    created_at: _timestamp_pb2.Timestamp
    def __init__(self, id: _Optional[str] = ..., tenant_id: _Optional[str] = ..., project_id: _Optional[str] = ..., name: _Optional[str] = ..., schema_version: _Optional[str] = ..., revision: _Optional[int] = ..., digest: _Optional[str] = ..., created_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class Ref(_message.Message):
    __slots__ = ("id", "digest")
    ID_FIELD_NUMBER: _ClassVar[int]
    DIGEST_FIELD_NUMBER: _ClassVar[int]
    id: str
    digest: str
    def __init__(self, id: _Optional[str] = ..., digest: _Optional[str] = ...) -> None: ...

class Limits(_message.Message):
    __slots__ = ("cpu", "memory_mb", "disk_mb", "timeout_seconds")
    CPU_FIELD_NUMBER: _ClassVar[int]
    MEMORY_MB_FIELD_NUMBER: _ClassVar[int]
    DISK_MB_FIELD_NUMBER: _ClassVar[int]
    TIMEOUT_SECONDS_FIELD_NUMBER: _ClassVar[int]
    cpu: int
    memory_mb: int
    disk_mb: int
    timeout_seconds: int
    def __init__(self, cpu: _Optional[int] = ..., memory_mb: _Optional[int] = ..., disk_mb: _Optional[int] = ..., timeout_seconds: _Optional[int] = ...) -> None: ...

class TaskSpec(_message.Message):
    __slots__ = ("workspace", "models", "gateway", "template_digest", "agent_digest", "dataset_digest", "command", "env", "limits", "execution_mode", "controlled_counter")
    class EnvEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    WORKSPACE_FIELD_NUMBER: _ClassVar[int]
    MODELS_FIELD_NUMBER: _ClassVar[int]
    GATEWAY_FIELD_NUMBER: _ClassVar[int]
    TEMPLATE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    AGENT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    DATASET_DIGEST_FIELD_NUMBER: _ClassVar[int]
    COMMAND_FIELD_NUMBER: _ClassVar[int]
    ENV_FIELD_NUMBER: _ClassVar[int]
    LIMITS_FIELD_NUMBER: _ClassVar[int]
    EXECUTION_MODE_FIELD_NUMBER: _ClassVar[int]
    CONTROLLED_COUNTER_FIELD_NUMBER: _ClassVar[int]
    workspace: Ref
    models: _containers.RepeatedCompositeFieldContainer[Ref]
    gateway: Ref
    template_digest: str
    agent_digest: str
    dataset_digest: str
    command: _containers.RepeatedScalarFieldContainer[str]
    env: _containers.ScalarMap[str, str]
    limits: Limits
    execution_mode: str
    controlled_counter: int
    def __init__(self, workspace: _Optional[_Union[Ref, _Mapping]] = ..., models: _Optional[_Iterable[_Union[Ref, _Mapping]]] = ..., gateway: _Optional[_Union[Ref, _Mapping]] = ..., template_digest: _Optional[str] = ..., agent_digest: _Optional[str] = ..., dataset_digest: _Optional[str] = ..., command: _Optional[_Iterable[str]] = ..., env: _Optional[_Mapping[str, str]] = ..., limits: _Optional[_Union[Limits, _Mapping]] = ..., execution_mode: _Optional[str] = ..., controlled_counter: _Optional[int] = ...) -> None: ...

class Repository(_message.Message):
    __slots__ = ("url", "commit")
    URL_FIELD_NUMBER: _ClassVar[int]
    COMMIT_FIELD_NUMBER: _ClassVar[int]
    url: str
    commit: str
    def __init__(self, url: _Optional[str] = ..., commit: _Optional[str] = ...) -> None: ...

class InputFile(_message.Message):
    __slots__ = ("path", "content", "digest")
    PATH_FIELD_NUMBER: _ClassVar[int]
    CONTENT_FIELD_NUMBER: _ClassVar[int]
    DIGEST_FIELD_NUMBER: _ClassVar[int]
    path: str
    content: bytes
    digest: str
    def __init__(self, path: _Optional[str] = ..., content: _Optional[bytes] = ..., digest: _Optional[str] = ...) -> None: ...

class WorkspaceSpec(_message.Message):
    __slots__ = ("repositories", "files", "private_writes")
    REPOSITORIES_FIELD_NUMBER: _ClassVar[int]
    FILES_FIELD_NUMBER: _ClassVar[int]
    PRIVATE_WRITES_FIELD_NUMBER: _ClassVar[int]
    repositories: _containers.RepeatedCompositeFieldContainer[Repository]
    files: _containers.RepeatedCompositeFieldContainer[InputFile]
    private_writes: bool
    def __init__(self, repositories: _Optional[_Iterable[_Union[Repository, _Mapping]]] = ..., files: _Optional[_Iterable[_Union[InputFile, _Mapping]]] = ..., private_writes: bool = ...) -> None: ...

class ModelConfigSpec(_message.Message):
    __slots__ = ("provider", "model", "revision", "artifact_digest", "parameters")
    class ParametersEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    PROVIDER_FIELD_NUMBER: _ClassVar[int]
    MODEL_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    ARTIFACT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    PARAMETERS_FIELD_NUMBER: _ClassVar[int]
    provider: str
    model: str
    revision: str
    artifact_digest: str
    parameters: _containers.ScalarMap[str, str]
    def __init__(self, provider: _Optional[str] = ..., model: _Optional[str] = ..., revision: _Optional[str] = ..., artifact_digest: _Optional[str] = ..., parameters: _Optional[_Mapping[str, str]] = ...) -> None: ...

class GatewaySpec(_message.Message):
    __slots__ = ("routes", "secret_refs", "allowed_hosts")
    class RoutesEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    ROUTES_FIELD_NUMBER: _ClassVar[int]
    SECRET_REFS_FIELD_NUMBER: _ClassVar[int]
    ALLOWED_HOSTS_FIELD_NUMBER: _ClassVar[int]
    routes: _containers.ScalarMap[str, str]
    secret_refs: _containers.RepeatedScalarFieldContainer[str]
    allowed_hosts: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, routes: _Optional[_Mapping[str, str]] = ..., secret_refs: _Optional[_Iterable[str]] = ..., allowed_hosts: _Optional[_Iterable[str]] = ...) -> None: ...

class RunSpec(_message.Message):
    __slots__ = ("task", "workspace", "models", "gateway")
    TASK_FIELD_NUMBER: _ClassVar[int]
    WORKSPACE_FIELD_NUMBER: _ClassVar[int]
    MODELS_FIELD_NUMBER: _ClassVar[int]
    GATEWAY_FIELD_NUMBER: _ClassVar[int]
    task: Resource
    workspace: Resource
    models: _containers.RepeatedCompositeFieldContainer[Resource]
    gateway: Resource
    def __init__(self, task: _Optional[_Union[Resource, _Mapping]] = ..., workspace: _Optional[_Union[Resource, _Mapping]] = ..., models: _Optional[_Iterable[_Union[Resource, _Mapping]]] = ..., gateway: _Optional[_Union[Resource, _Mapping]] = ...) -> None: ...

class BranchSpec(_message.Message):
    __slots__ = ("run_id", "model", "parent_checkpoint_id", "parent_branch_id", "fork_operation_id")
    RUN_ID_FIELD_NUMBER: _ClassVar[int]
    MODEL_FIELD_NUMBER: _ClassVar[int]
    PARENT_CHECKPOINT_ID_FIELD_NUMBER: _ClassVar[int]
    PARENT_BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    FORK_OPERATION_ID_FIELD_NUMBER: _ClassVar[int]
    run_id: str
    model: Resource
    parent_checkpoint_id: str
    parent_branch_id: str
    fork_operation_id: str
    def __init__(self, run_id: _Optional[str] = ..., model: _Optional[_Union[Resource, _Mapping]] = ..., parent_checkpoint_id: _Optional[str] = ..., parent_branch_id: _Optional[str] = ..., fork_operation_id: _Optional[str] = ...) -> None: ...

class CheckpointSpec(_message.Message):
    __slots__ = ("manifest_digest", "manifest_uri", "protocol_version", "event_position", "verified", "branch_id", "compatibility", "native_checkpoint_id", "snapshot_manifest", "barrier", "route_digest", "budget_remaining_ms")
    class CompatibilityEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    MANIFEST_DIGEST_FIELD_NUMBER: _ClassVar[int]
    MANIFEST_URI_FIELD_NUMBER: _ClassVar[int]
    PROTOCOL_VERSION_FIELD_NUMBER: _ClassVar[int]
    EVENT_POSITION_FIELD_NUMBER: _ClassVar[int]
    VERIFIED_FIELD_NUMBER: _ClassVar[int]
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    COMPATIBILITY_FIELD_NUMBER: _ClassVar[int]
    NATIVE_CHECKPOINT_ID_FIELD_NUMBER: _ClassVar[int]
    SNAPSHOT_MANIFEST_FIELD_NUMBER: _ClassVar[int]
    BARRIER_FIELD_NUMBER: _ClassVar[int]
    ROUTE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    BUDGET_REMAINING_MS_FIELD_NUMBER: _ClassVar[int]
    manifest_digest: str
    manifest_uri: str
    protocol_version: str
    event_position: int
    verified: bool
    branch_id: str
    compatibility: _containers.ScalarMap[str, str]
    native_checkpoint_id: str
    snapshot_manifest: bytes
    barrier: _harness_pb2.AgentBarrier
    route_digest: str
    budget_remaining_ms: int
    def __init__(self, manifest_digest: _Optional[str] = ..., manifest_uri: _Optional[str] = ..., protocol_version: _Optional[str] = ..., event_position: _Optional[int] = ..., verified: bool = ..., branch_id: _Optional[str] = ..., compatibility: _Optional[_Mapping[str, str]] = ..., native_checkpoint_id: _Optional[str] = ..., snapshot_manifest: _Optional[bytes] = ..., barrier: _Optional[_Union[_harness_pb2.AgentBarrier, _Mapping]] = ..., route_digest: _Optional[str] = ..., budget_remaining_ms: _Optional[int] = ...) -> None: ...

class OperationSpec(_message.Message):
    __slots__ = ("branch_id", "action", "checkpoint_id", "child_branch_ids", "parent_operation_id")
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    ACTION_FIELD_NUMBER: _ClassVar[int]
    CHECKPOINT_ID_FIELD_NUMBER: _ClassVar[int]
    CHILD_BRANCH_IDS_FIELD_NUMBER: _ClassVar[int]
    PARENT_OPERATION_ID_FIELD_NUMBER: _ClassVar[int]
    branch_id: str
    action: str
    checkpoint_id: str
    child_branch_ids: _containers.RepeatedScalarFieldContainer[str]
    parent_operation_id: str
    def __init__(self, branch_id: _Optional[str] = ..., action: _Optional[str] = ..., checkpoint_id: _Optional[str] = ..., child_branch_ids: _Optional[_Iterable[str]] = ..., parent_operation_id: _Optional[str] = ...) -> None: ...

class BranchStatus(_message.Message):
    __slots__ = ("desired", "observed", "execution_id", "epoch", "lease_until", "last_event", "error", "checkpoint_id", "budget_remaining_ms", "sandbox_id", "agent_gate", "route_digest", "agent_step", "trace_complete")
    DESIRED_FIELD_NUMBER: _ClassVar[int]
    OBSERVED_FIELD_NUMBER: _ClassVar[int]
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    EPOCH_FIELD_NUMBER: _ClassVar[int]
    LEASE_UNTIL_FIELD_NUMBER: _ClassVar[int]
    LAST_EVENT_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    CHECKPOINT_ID_FIELD_NUMBER: _ClassVar[int]
    BUDGET_REMAINING_MS_FIELD_NUMBER: _ClassVar[int]
    SANDBOX_ID_FIELD_NUMBER: _ClassVar[int]
    AGENT_GATE_FIELD_NUMBER: _ClassVar[int]
    ROUTE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    AGENT_STEP_FIELD_NUMBER: _ClassVar[int]
    TRACE_COMPLETE_FIELD_NUMBER: _ClassVar[int]
    desired: str
    observed: str
    execution_id: str
    epoch: int
    lease_until: _timestamp_pb2.Timestamp
    last_event: int
    error: str
    checkpoint_id: str
    budget_remaining_ms: int
    sandbox_id: str
    agent_gate: str
    route_digest: str
    agent_step: int
    trace_complete: bool
    def __init__(self, desired: _Optional[str] = ..., observed: _Optional[str] = ..., execution_id: _Optional[str] = ..., epoch: _Optional[int] = ..., lease_until: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., last_event: _Optional[int] = ..., error: _Optional[str] = ..., checkpoint_id: _Optional[str] = ..., budget_remaining_ms: _Optional[int] = ..., sandbox_id: _Optional[str] = ..., agent_gate: _Optional[str] = ..., route_digest: _Optional[str] = ..., agent_step: _Optional[int] = ..., trace_complete: bool = ...) -> None: ...

class OperationStatus(_message.Message):
    __slots__ = ("state", "error")
    STATE_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    state: str
    error: str
    def __init__(self, state: _Optional[str] = ..., error: _Optional[str] = ...) -> None: ...

class Resource(_message.Message):
    __slots__ = ("metadata", "kind", "task_spec", "workspace_spec", "model_config_spec", "gateway_spec", "run_spec", "branch_spec", "checkpoint_spec", "operation_spec", "branch_status", "operation_status")
    METADATA_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    TASK_SPEC_FIELD_NUMBER: _ClassVar[int]
    WORKSPACE_SPEC_FIELD_NUMBER: _ClassVar[int]
    MODEL_CONFIG_SPEC_FIELD_NUMBER: _ClassVar[int]
    GATEWAY_SPEC_FIELD_NUMBER: _ClassVar[int]
    RUN_SPEC_FIELD_NUMBER: _ClassVar[int]
    BRANCH_SPEC_FIELD_NUMBER: _ClassVar[int]
    CHECKPOINT_SPEC_FIELD_NUMBER: _ClassVar[int]
    OPERATION_SPEC_FIELD_NUMBER: _ClassVar[int]
    BRANCH_STATUS_FIELD_NUMBER: _ClassVar[int]
    OPERATION_STATUS_FIELD_NUMBER: _ClassVar[int]
    metadata: Metadata
    kind: str
    task_spec: TaskSpec
    workspace_spec: WorkspaceSpec
    model_config_spec: ModelConfigSpec
    gateway_spec: GatewaySpec
    run_spec: RunSpec
    branch_spec: BranchSpec
    checkpoint_spec: CheckpointSpec
    operation_spec: OperationSpec
    branch_status: BranchStatus
    operation_status: OperationStatus
    def __init__(self, metadata: _Optional[_Union[Metadata, _Mapping]] = ..., kind: _Optional[str] = ..., task_spec: _Optional[_Union[TaskSpec, _Mapping]] = ..., workspace_spec: _Optional[_Union[WorkspaceSpec, _Mapping]] = ..., model_config_spec: _Optional[_Union[ModelConfigSpec, _Mapping]] = ..., gateway_spec: _Optional[_Union[GatewaySpec, _Mapping]] = ..., run_spec: _Optional[_Union[RunSpec, _Mapping]] = ..., branch_spec: _Optional[_Union[BranchSpec, _Mapping]] = ..., checkpoint_spec: _Optional[_Union[CheckpointSpec, _Mapping]] = ..., operation_spec: _Optional[_Union[OperationSpec, _Mapping]] = ..., branch_status: _Optional[_Union[BranchStatus, _Mapping]] = ..., operation_status: _Optional[_Union[OperationStatus, _Mapping]] = ...) -> None: ...

class PutResourceRequest(_message.Message):
    __slots__ = ("project_id", "resource", "idempotency_key")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    RESOURCE_FIELD_NUMBER: _ClassVar[int]
    IDEMPOTENCY_KEY_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    resource: Resource
    idempotency_key: str
    def __init__(self, project_id: _Optional[str] = ..., resource: _Optional[_Union[Resource, _Mapping]] = ..., idempotency_key: _Optional[str] = ...) -> None: ...

class GetResourceRequest(_message.Message):
    __slots__ = ("project_id", "kind", "id")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    kind: str
    id: str
    def __init__(self, project_id: _Optional[str] = ..., kind: _Optional[str] = ..., id: _Optional[str] = ...) -> None: ...

class ListResourcesRequest(_message.Message):
    __slots__ = ("project_id", "kind", "after", "limit")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    AFTER_FIELD_NUMBER: _ClassVar[int]
    LIMIT_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    kind: str
    after: str
    limit: int
    def __init__(self, project_id: _Optional[str] = ..., kind: _Optional[str] = ..., after: _Optional[str] = ..., limit: _Optional[int] = ...) -> None: ...

class ListResourcesResponse(_message.Message):
    __slots__ = ("resources", "next_after")
    RESOURCES_FIELD_NUMBER: _ClassVar[int]
    NEXT_AFTER_FIELD_NUMBER: _ClassVar[int]
    resources: _containers.RepeatedCompositeFieldContainer[Resource]
    next_after: str
    def __init__(self, resources: _Optional[_Iterable[_Union[Resource, _Mapping]]] = ..., next_after: _Optional[str] = ...) -> None: ...

class SubmitRequest(_message.Message):
    __slots__ = ("project_id", "task_id", "idempotency_key")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    TASK_ID_FIELD_NUMBER: _ClassVar[int]
    IDEMPOTENCY_KEY_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    task_id: str
    idempotency_key: str
    def __init__(self, project_id: _Optional[str] = ..., task_id: _Optional[str] = ..., idempotency_key: _Optional[str] = ...) -> None: ...

class SubmitManifestRequest(_message.Message):
    __slots__ = ("project_id", "manifest_yaml", "idempotency_key")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    MANIFEST_YAML_FIELD_NUMBER: _ClassVar[int]
    IDEMPOTENCY_KEY_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    manifest_yaml: str
    idempotency_key: str
    def __init__(self, project_id: _Optional[str] = ..., manifest_yaml: _Optional[str] = ..., idempotency_key: _Optional[str] = ...) -> None: ...

class Submission(_message.Message):
    __slots__ = ("run", "branch", "operation")
    RUN_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    OPERATION_FIELD_NUMBER: _ClassVar[int]
    run: Resource
    branch: Resource
    operation: Resource
    def __init__(self, run: _Optional[_Union[Resource, _Mapping]] = ..., branch: _Optional[_Union[Resource, _Mapping]] = ..., operation: _Optional[_Union[Resource, _Mapping]] = ...) -> None: ...

class ActionRequest(_message.Message):
    __slots__ = ("project_id", "branch_id", "action", "idempotency_key")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    ACTION_FIELD_NUMBER: _ClassVar[int]
    IDEMPOTENCY_KEY_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    branch_id: str
    action: str
    idempotency_key: str
    def __init__(self, project_id: _Optional[str] = ..., branch_id: _Optional[str] = ..., action: _Optional[str] = ..., idempotency_key: _Optional[str] = ...) -> None: ...

class Event(_message.Message):
    __slots__ = ("id", "branch_id", "sequence", "epoch", "type", "data", "created_at")
    ID_FIELD_NUMBER: _ClassVar[int]
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    EPOCH_FIELD_NUMBER: _ClassVar[int]
    TYPE_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    id: str
    branch_id: str
    sequence: int
    epoch: int
    type: str
    data: _struct_pb2.Struct
    created_at: _timestamp_pb2.Timestamp
    def __init__(self, id: _Optional[str] = ..., branch_id: _Optional[str] = ..., sequence: _Optional[int] = ..., epoch: _Optional[int] = ..., type: _Optional[str] = ..., data: _Optional[_Union[_struct_pb2.Struct, _Mapping]] = ..., created_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class EventsRequest(_message.Message):
    __slots__ = ("project_id", "branch_id", "after_sequence", "limit")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    AFTER_SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    LIMIT_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    branch_id: str
    after_sequence: int
    limit: int
    def __init__(self, project_id: _Optional[str] = ..., branch_id: _Optional[str] = ..., after_sequence: _Optional[int] = ..., limit: _Optional[int] = ...) -> None: ...

class EventsResponse(_message.Message):
    __slots__ = ("events", "next_sequence")
    EVENTS_FIELD_NUMBER: _ClassVar[int]
    NEXT_SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    events: _containers.RepeatedCompositeFieldContainer[Event]
    next_sequence: int
    def __init__(self, events: _Optional[_Iterable[_Union[Event, _Mapping]]] = ..., next_sequence: _Optional[int] = ...) -> None: ...

class ExecRequest(_message.Message):
    __slots__ = ("project_id", "branch_id", "command")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    COMMAND_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    branch_id: str
    command: _guest_pb2.ExecRequest
    def __init__(self, project_id: _Optional[str] = ..., branch_id: _Optional[str] = ..., command: _Optional[_Union[_guest_pb2.ExecRequest, _Mapping]] = ...) -> None: ...

class ReadFileRequest(_message.Message):
    __slots__ = ("project_id", "branch_id", "path")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    PATH_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    branch_id: str
    path: str
    def __init__(self, project_id: _Optional[str] = ..., branch_id: _Optional[str] = ..., path: _Optional[str] = ...) -> None: ...

class WriteFileRequest(_message.Message):
    __slots__ = ("project_id", "branch_id", "path", "data")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    PATH_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    branch_id: str
    path: str
    data: bytes
    def __init__(self, project_id: _Optional[str] = ..., branch_id: _Optional[str] = ..., path: _Optional[str] = ..., data: _Optional[bytes] = ...) -> None: ...

class ForkRequest(_message.Message):
    __slots__ = ("project_id", "checkpoint_id", "models", "idempotency_key")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    CHECKPOINT_ID_FIELD_NUMBER: _ClassVar[int]
    MODELS_FIELD_NUMBER: _ClassVar[int]
    IDEMPOTENCY_KEY_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    checkpoint_id: str
    models: _containers.RepeatedCompositeFieldContainer[Ref]
    idempotency_key: str
    def __init__(self, project_id: _Optional[str] = ..., checkpoint_id: _Optional[str] = ..., models: _Optional[_Iterable[_Union[Ref, _Mapping]]] = ..., idempotency_key: _Optional[str] = ...) -> None: ...

class ForkResult(_message.Message):
    __slots__ = ("operation", "children")
    OPERATION_FIELD_NUMBER: _ClassVar[int]
    CHILDREN_FIELD_NUMBER: _ClassVar[int]
    operation: Resource
    children: _containers.RepeatedCompositeFieldContainer[Resource]
    def __init__(self, operation: _Optional[_Union[Resource, _Mapping]] = ..., children: _Optional[_Iterable[_Union[Resource, _Mapping]]] = ...) -> None: ...

class CallsRequest(_message.Message):
    __slots__ = ("project_id", "branch_id", "after_sequence", "limit")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    AFTER_SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    LIMIT_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    branch_id: str
    after_sequence: int
    limit: int
    def __init__(self, project_id: _Optional[str] = ..., branch_id: _Optional[str] = ..., after_sequence: _Optional[int] = ..., limit: _Optional[int] = ...) -> None: ...

class AgentCall(_message.Message):
    __slots__ = ("request", "result", "execution_id", "epoch", "model_digest", "route_digest", "adapter_version", "mutating", "created_at", "registered_sequence")
    REQUEST_FIELD_NUMBER: _ClassVar[int]
    RESULT_FIELD_NUMBER: _ClassVar[int]
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    EPOCH_FIELD_NUMBER: _ClassVar[int]
    MODEL_DIGEST_FIELD_NUMBER: _ClassVar[int]
    ROUTE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    ADAPTER_VERSION_FIELD_NUMBER: _ClassVar[int]
    MUTATING_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    REGISTERED_SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    request: _harness_pb2.AgentRequest
    result: _harness_pb2.AgentResult
    execution_id: str
    epoch: int
    model_digest: str
    route_digest: str
    adapter_version: str
    mutating: bool
    created_at: _timestamp_pb2.Timestamp
    registered_sequence: int
    def __init__(self, request: _Optional[_Union[_harness_pb2.AgentRequest, _Mapping]] = ..., result: _Optional[_Union[_harness_pb2.AgentResult, _Mapping]] = ..., execution_id: _Optional[str] = ..., epoch: _Optional[int] = ..., model_digest: _Optional[str] = ..., route_digest: _Optional[str] = ..., adapter_version: _Optional[str] = ..., mutating: bool = ..., created_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., registered_sequence: _Optional[int] = ...) -> None: ...

class CallsResponse(_message.Message):
    __slots__ = ("calls", "next_sequence")
    CALLS_FIELD_NUMBER: _ClassVar[int]
    NEXT_SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    calls: _containers.RepeatedCompositeFieldContainer[AgentCall]
    next_sequence: int
    def __init__(self, calls: _Optional[_Iterable[_Union[AgentCall, _Mapping]]] = ..., next_sequence: _Optional[int] = ...) -> None: ...

class StepsRequest(_message.Message):
    __slots__ = ("project_id", "branch_id", "after_step", "limit")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    AFTER_STEP_FIELD_NUMBER: _ClassVar[int]
    LIMIT_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    branch_id: str
    after_step: int
    limit: int
    def __init__(self, project_id: _Optional[str] = ..., branch_id: _Optional[str] = ..., after_step: _Optional[int] = ..., limit: _Optional[int] = ...) -> None: ...

class AgentStep(_message.Message):
    __slots__ = ("branch_id", "step", "state_digest", "event_sequence", "session_hash", "complete")
    BRANCH_ID_FIELD_NUMBER: _ClassVar[int]
    STEP_FIELD_NUMBER: _ClassVar[int]
    STATE_DIGEST_FIELD_NUMBER: _ClassVar[int]
    EVENT_SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    SESSION_HASH_FIELD_NUMBER: _ClassVar[int]
    COMPLETE_FIELD_NUMBER: _ClassVar[int]
    branch_id: str
    step: int
    state_digest: str
    event_sequence: int
    session_hash: str
    complete: bool
    def __init__(self, branch_id: _Optional[str] = ..., step: _Optional[int] = ..., state_digest: _Optional[str] = ..., event_sequence: _Optional[int] = ..., session_hash: _Optional[str] = ..., complete: bool = ...) -> None: ...

class StepsResponse(_message.Message):
    __slots__ = ("steps", "next_step")
    STEPS_FIELD_NUMBER: _ClassVar[int]
    NEXT_STEP_FIELD_NUMBER: _ClassVar[int]
    steps: _containers.RepeatedCompositeFieldContainer[AgentStep]
    next_step: int
    def __init__(self, steps: _Optional[_Iterable[_Union[AgentStep, _Mapping]]] = ..., next_step: _Optional[int] = ...) -> None: ...

class CompareRequest(_message.Message):
    __slots__ = ("project_id", "left_branch", "right_branch", "comparator")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    LEFT_BRANCH_FIELD_NUMBER: _ClassVar[int]
    RIGHT_BRANCH_FIELD_NUMBER: _ClassVar[int]
    COMPARATOR_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    left_branch: str
    right_branch: str
    comparator: str
    def __init__(self, project_id: _Optional[str] = ..., left_branch: _Optional[str] = ..., right_branch: _Optional[str] = ..., comparator: _Optional[str] = ...) -> None: ...

class Divergence(_message.Message):
    __slots__ = ("comparator", "state", "step", "position", "left_digest", "right_digest", "reason")
    COMPARATOR_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    STEP_FIELD_NUMBER: _ClassVar[int]
    POSITION_FIELD_NUMBER: _ClassVar[int]
    LEFT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    RIGHT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    comparator: str
    state: str
    step: int
    position: int
    left_digest: str
    right_digest: str
    reason: str
    def __init__(self, comparator: _Optional[str] = ..., state: _Optional[str] = ..., step: _Optional[int] = ..., position: _Optional[int] = ..., left_digest: _Optional[str] = ..., right_digest: _Optional[str] = ..., reason: _Optional[str] = ...) -> None: ...
