// Copyright 2025 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package model

const (
	// ApiAccessTokenHeader carries the auth token.
	ApiAccessTokenHeader = "X-EXECD-ACCESS-TOKEN"
	// SessionCapabilityHeader carries the one-time secret returned when an
	// isolated session is created.
	SessionCapabilityHeader = "X-OpenSandbox-Session-Capability"
	// ManagerControlHeaderPrefix reserves signed manager-to-execd control
	// headers. They must never be forwarded to a user workload.
	ManagerControlHeaderPrefix = "X-EXECD-MANAGER-"
	// LegacyManagerControlHeaderPrefix is stripped as a defense-in-depth
	// compatibility boundary for early manager integrations.
	LegacyManagerControlHeaderPrefix = "X-OpenSandbox-Manager-"
)
