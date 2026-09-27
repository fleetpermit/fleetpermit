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

package v1alpha1

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestLeaseImmutabilityRuleCoversEverySpecField checks that the generated
// CRD's immutability rule compares every ToolAccessLeaseSpec field, so a
// field added to the spec cannot be changed after creation by accident.
func TestLeaseImmutabilityRuleCoversEverySpecField(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "fleetpermit.github.io_toolaccessleases.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Validations []struct{ Rule, Message string } `json:"x-kubernetes-validations"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(b, &crd); err != nil {
		t.Fatal(err)
	}
	var rule string
	for _, v := range crd.Spec.Versions {
		for _, r := range v.Schema.OpenAPIV3Schema.Properties.Spec.Validations {
			if strings.Contains(r.Message, "immutable") {
				rule = r.Rule
			}
		}
	}
	if rule == "" {
		t.Fatal("no immutability rule on spec in the generated CRD")
	}
	spec := reflect.TypeOf(ToolAccessLeaseSpec{})
	for i := 0; i < spec.NumField(); i++ {
		tag, _, _ := strings.Cut(spec.Field(i).Tag.Get("json"), ",")
		for _, ref := range []string{"self." + tag, "oldSelf." + tag} {
			if !strings.Contains(rule, ref) {
				t.Errorf("the immutability rule does not compare %s (%s): %s", spec.Field(i).Name, ref, rule)
			}
		}
	}
}
