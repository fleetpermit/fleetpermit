/*
Copyright The FleetPermit Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package digest

import (
	"strings"
	"testing"
)

type content struct {
	Cluster string            `json:"cluster"`
	Tools   []string          `json:"tools"`
	Labels  map[string]string `json:"labels"`
}

func TestOfIsDeterministic(t *testing.T) {
	a := content{Cluster: "east", Tools: []string{"a", "b"}, Labels: map[string]string{"x": "1", "y": "2"}}
	b := content{Cluster: "east", Tools: []string{"a", "b"}, Labels: map[string]string{"y": "2", "x": "1"}}
	da, err := Of(a)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		db, err := Of(b)
		if err != nil {
			t.Fatal(err)
		}
		if da != db {
			t.Fatalf("digest changed between runs: %s vs %s", da, db)
		}
	}
}

func TestOfDetectsChanges(t *testing.T) {
	base := content{Cluster: "east", Tools: []string{"a"}}
	d0, _ := Of(base)
	for name, v := range map[string]content{
		"cluster": {Cluster: "west", Tools: []string{"a"}},
		"tool":    {Cluster: "east", Tools: []string{"b"}},
		"extra":   {Cluster: "east", Tools: []string{"a", "b"}},
	} {
		d, _ := Of(v)
		if d == d0 {
			t.Errorf("%s: digest did not change", name)
		}
	}
}

func TestOfFormat(t *testing.T) {
	d, err := Of(content{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d, Prefix) || len(d) != len(Prefix)+64 {
		t.Fatalf("unexpected digest format %q", d)
	}
	// Known-answer vector, computed independently with
	// printf '{"cluster":"","tools":null,"labels":null}' | shasum -a 256
	const want = "sha256:b1fe05fb658580fa4a0936f53fa8def49a8b0f023a9f151a5792eac5fc9f8237"
	if d != want {
		t.Fatalf("digest = %s, want %s", d, want)
	}
}

func TestOfRejectsUnencodable(t *testing.T) {
	if _, err := Of(make(chan int)); err == nil {
		t.Fatal("expected an error for an unencodable value")
	}
}

func TestShort(t *testing.T) {
	if got := Short("fleet/sre", 8); len(got) != 8 || got != Short("fleet/sre", 8) {
		t.Fatalf("Short is not stable: %q", got)
	}
	if Short("a", 8) == Short("b", 8) {
		t.Fatal("different inputs produced the same short hash")
	}
	if got := Short("a", 1000); len(got) != 64 {
		t.Fatalf("Short must cap at the full hash length, got %d", len(got))
	}
}
