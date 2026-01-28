// Copyright 2026 Alibaba Group Holding Ltd.
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

package policy

import "testing"

func TestParsePolicy_EmptyOrNullDefaultsDeny(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"null",
		"{}\n",
	}
	for _, raw := range cases {
		p, err := ParsePolicy(raw)
		if err != nil {
			t.Fatalf("raw %q returned error: %v", raw, err)
		}
		if p == nil {
			t.Fatalf("raw %q expected default deny policy, got nil", raw)
		}
		if p.DefaultAction != ActionDeny {
			t.Fatalf("raw %q expected defaultAction deny, got %+v", raw, p)
		}
		if got := p.Evaluate("example.com."); got != ActionDeny {
			t.Fatalf("raw %q expected deny evaluation, got %s", raw, got)
		}
	}
}

func TestParsePolicy_DefaultActionFallback(t *testing.T) {
	p, err := ParsePolicy(`{"egress":[{"action":"allow","target":"example.com"}]}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatalf("expected policy object, got nil")
	}
	if p.DefaultAction != ActionDeny {
		t.Fatalf("expected defaultAction fallback to deny, got %+v", p)
	}
}

func TestParsePolicy_EmptyEgressDefaultsDeny(t *testing.T) {
	p, err := ParsePolicy(`{"defaultAction":""}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.DefaultAction != ActionDeny {
		t.Fatalf("expected default deny when defaultAction missing, got %+v", p)
	}
	if got := p.Evaluate("anything.com."); got != ActionDeny {
		t.Fatalf("expected evaluation deny for empty egress, got %s", got)
	}
}
