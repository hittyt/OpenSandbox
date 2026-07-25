#
# Copyright 2026 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

import httpx

from opensandbox.api.execd import Client
from opensandbox.api.execd.api.isolated_execution import (
    create_isolated_session,
    delete_isolated_session,
)
from opensandbox.api.execd.models.error_response import ErrorResponse
from opensandbox.api.execd.models.isolated_create_session_response import (
    IsolatedCreateSessionResponse,
)
from opensandbox.api.execd.types import UNSET


def test_create_session_response_accepts_legacy_response_without_capability() -> None:
    response = IsolatedCreateSessionResponse.from_dict(
        {
            "session_id": "8d734e9e-b701-4b39-8f09-55b12a8eb5a2",
            "created_at": "2026-07-25T06:10:00Z",
        }
    )

    assert response.capability is UNSET


def test_create_session_response_exposes_capability_without_repr_leak() -> None:
    raw_capability = "sensitive-session-capability"
    response = IsolatedCreateSessionResponse.from_dict(
        {
            "session_id": "8d734e9e-b701-4b39-8f09-55b12a8eb5a2",
            "created_at": "2026-07-25T06:10:00Z",
            "capability": raw_capability,
        }
    )

    assert response.capability == raw_capability
    assert raw_capability not in repr(response)


def test_create_and_delete_parse_documented_runtime_errors() -> None:
    client = Client(base_url="http://execd.test")
    for parse_response in (
        create_isolated_session._parse_response,
        delete_isolated_session._parse_response,
    ):
        parsed = parse_response(
            client=client,
            response=httpx.Response(
                500,
                json={
                    "code": "RUNTIME_ERROR",
                    "message": "isolated session operation failed",
                },
            ),
        )

        assert isinstance(parsed, ErrorResponse)
        assert parsed.code == "RUNTIME_ERROR"
