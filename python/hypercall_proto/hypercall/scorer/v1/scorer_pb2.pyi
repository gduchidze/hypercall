from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ScoreRequest(_message.Message):
    __slots__ = ("output", "variables", "scorer_digest", "rubric_digest", "judge_model_digest", "parameters")
    class VariablesEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    class ParametersEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    OUTPUT_FIELD_NUMBER: _ClassVar[int]
    VARIABLES_FIELD_NUMBER: _ClassVar[int]
    SCORER_DIGEST_FIELD_NUMBER: _ClassVar[int]
    RUBRIC_DIGEST_FIELD_NUMBER: _ClassVar[int]
    JUDGE_MODEL_DIGEST_FIELD_NUMBER: _ClassVar[int]
    PARAMETERS_FIELD_NUMBER: _ClassVar[int]
    output: str
    variables: _containers.ScalarMap[str, str]
    scorer_digest: str
    rubric_digest: str
    judge_model_digest: str
    parameters: _containers.ScalarMap[str, str]
    def __init__(self, output: _Optional[str] = ..., variables: _Optional[_Mapping[str, str]] = ..., scorer_digest: _Optional[str] = ..., rubric_digest: _Optional[str] = ..., judge_model_digest: _Optional[str] = ..., parameters: _Optional[_Mapping[str, str]] = ...) -> None: ...

class Cost(_message.Message):
    __slots__ = ("amount", "currency", "kind", "source")
    AMOUNT_FIELD_NUMBER: _ClassVar[int]
    CURRENCY_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    SOURCE_FIELD_NUMBER: _ClassVar[int]
    amount: float
    currency: str
    kind: str
    source: str
    def __init__(self, amount: _Optional[float] = ..., currency: _Optional[str] = ..., kind: _Optional[str] = ..., source: _Optional[str] = ...) -> None: ...

class ScoreResult(_message.Message):
    __slots__ = ("score", "reason", "error_code", "cost")
    PASS_FIELD_NUMBER: _ClassVar[int]
    SCORE_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    ERROR_CODE_FIELD_NUMBER: _ClassVar[int]
    COST_FIELD_NUMBER: _ClassVar[int]
    score: float
    reason: str
    error_code: str
    cost: Cost
    def __init__(self, score: _Optional[float] = ..., reason: _Optional[str] = ..., error_code: _Optional[str] = ..., cost: _Optional[_Union[Cost, _Mapping]] = ..., **kwargs) -> None: ...
