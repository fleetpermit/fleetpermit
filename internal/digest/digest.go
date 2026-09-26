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

// Package digest computes stable content digests that bind a rendered
// enforcement object to the FleetPermit inputs that produced it.
package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Prefix identifies the hash algorithm in a digest string.
const Prefix = "sha256:"

// Of returns "sha256:<hex>" over the JSON encoding of v.
//
// encoding/json writes struct fields in declaration order and map keys in
// sorted order, so the result is deterministic for a given value. Callers are
// responsible for sorting slices whose order carries no meaning.
func Of(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encoding value for digest: %w", err)
	}
	sum := sha256.Sum256(b)
	return Prefix + hex.EncodeToString(sum[:]), nil
}

// Short returns the first n hex characters of the SHA-256 of s. It is used to
// derive stable, bounded-length Kubernetes names, not for integrity.
func Short(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	h := hex.EncodeToString(sum[:])
	if n > len(h) {
		n = len(h)
	}
	return h[:n]
}
