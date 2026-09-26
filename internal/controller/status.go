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

package controller

import (
	"context"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
)

func setCond(conds *[]metav1.Condition, gen int64, t string, status bool, reason, msg string) {
	s := metav1.ConditionFalse
	if status {
		s = metav1.ConditionTrue
	}
	if reason == "" {
		reason = fpv1.ReasonReconciled
	}
	meta.SetStatusCondition(conds, metav1.Condition{
		Type: t, Status: s, Reason: reason, Message: truncate(msg, 1024), ObservedGeneration: gen,
	})
}

func findCond(conds []metav1.Condition, t string) *metav1.Condition {
	return meta.FindStatusCondition(conds, t)
}

func hasTrue(conds []metav1.Condition, t string) bool {
	return meta.IsStatusConditionTrue(conds, t)
}

func conditionTime(conds []metav1.Condition, t string, fallback time.Time) time.Time {
	if c := findCond(conds, t); c != nil && c.Status == metav1.ConditionTrue {
		return c.LastTransitionTime.Time
	}
	return fallback
}

func metav1Time(t time.Time) metav1.Time { return metav1.NewTime(t.UTC()) }

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// patchLeaseStatus writes the status only when it changed. The patch is
// conditional on the resource version that was read, so a status computed
// from a stale cache never overwrites a newer one, such as a terminal state;
// a conflict is returned and the policy is reconciled again.
func (r *PolicyReconciler) patchLeaseStatus(ctx context.Context, l *fpv1.ToolAccessLease, st *fpv1.ToolAccessLeaseStatus) error {
	if equality.Semantic.DeepEqual(&l.Status, st) {
		return nil
	}
	base := l.DeepCopy()
	l.Status = *st
	return r.Status().Patch(ctx, l, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// patchPolicyStatus writes the status only when it changed.
func (r *PolicyReconciler) patchPolicyStatus(ctx context.Context, p *fpv1.FleetAccessPolicy, st *fpv1.FleetAccessPolicyStatus) error {
	if equality.Semantic.DeepEqual(&p.Status, st) {
		return nil
	}
	base := p.DeepCopy()
	p.Status = *st
	return r.Status().Patch(ctx, p, client.MergeFrom(base))
}
