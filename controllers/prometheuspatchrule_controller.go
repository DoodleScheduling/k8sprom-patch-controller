/*
Copyright 2022 Doodle.

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

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/doodlescheduling/k8sprom-patch-controller/api/v1beta1"
)

//+kubebuilder:rbac:groups=metrics.infra.doodle.com,resources=prometheuspatchrules,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=metrics.infra.doodle.com,resources=prometheuspatchrules/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=metrics.infra.doodle.com,resources=prometheuspatchrules/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// PatchPrometheusPatchRuleReconciler reconciles a PrometheusPatchRule object
type PrometheusPatchRuleReconciler struct {
	client.Client
	DynClient    dynamic.Interface
	FieldManager string
	Log          logr.Logger
	Recorder     record.EventRecorder
	Scheme       *runtime.Scheme
}

// PodReconcilerOptions
type PrometheusPatchRuleReconcilerOptions struct {
	MaxConcurrentReconciles int
}

// SetupWithManager sets up the controller with the Manager.
func (r *PrometheusPatchRuleReconciler) SetupWithManager(mgr ctrl.Manager, opts PrometheusPatchRuleReconcilerOptions) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.PrometheusPatchRule{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: opts.MaxConcurrentReconciles}).
		Complete(r)
}

// Reconcile PrometheusPatchRule
func (r *PrometheusPatchRuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Log.WithValues("Namespace", req.Namespace, "Name", req.NamespacedName)
	logger.Info("reconciling PrometheusPatchRule")

	// Fetch the Rule instance
	rule := v1beta1.PrometheusPatchRule{}

	err := r.Client.Get(ctx, req.NamespacedName, &rule)
	if err != nil {
		if kerrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	if rule.Spec.Suspend {
		return ctrl.Result{}, nil
	}

	rule, res, err := r.reconcile(ctx, rule, logger)
	if err != nil {
		r.Recorder.Event(&rule, "Normal", "error", err.Error())
	}

	// Update status after reconciliation.
	if err = r.patchStatus(ctx, &rule); err != nil {
		logger.Error(err, "unable to update status after reconciliation")
		return ctrl.Result{Requeue: true}, err
	}

	return res, err
}

func (r *PrometheusPatchRuleReconciler) reconcile(ctx context.Context, rule v1beta1.PrometheusPatchRule, logger logr.Logger) (v1beta1.PrometheusPatchRule, ctrl.Result, error) {
	client, err := api.NewClient(api.Config{
		Address: rule.Spec.Prometheus.Address,
	})

	if err != nil {
		err = fmt.Errorf("failed parse prometheus address: %w", err)
		rule = v1beta1.PrometheusPatchRuleNotActive(rule, v1beta1.InvalidPrometheusURLReason, err.Error())
		return rule, ctrl.Result{}, err
	}

	v1api := v1.NewAPI(client)
	result, warnings, err := v1api.Query(ctx, rule.Spec.Expr, time.Now())
	if err != nil {
		err = fmt.Errorf("failed executing prometheus query: %w", err)
		rule = v1beta1.PrometheusPatchRuleNotActive(rule, v1beta1.PrometheusQueryFailedReason, err.Error())
		return rule, ctrl.Result{}, err
	}

	if len(warnings) > 0 {
		logger.Info("detected prometheus query warnings", "warnings", warnings)
	}

	value, err := r.parseValue(result)
	if err != nil {
		err = fmt.Errorf("failed parsing metric value: %w", err)
		rule = v1beta1.PrometheusPatchRuleNotActive(rule, v1beta1.FailedReason, err.Error())
		return rule, ctrl.Result{}, err
	}

	if len(value) > 0 {
		msg := "found query samples"
		activeCondition := meta.FindStatusCondition(rule.Status.Conditions, v1beta1.ActiveCondition)
		if activeCondition == nil {
			activeCondition = &metav1.Condition{}
		}

		// If we have waiting window (spec.for) add pending condition reason
		if activeCondition.Reason != v1beta1.PendingReason && activeCondition.Reason != v1beta1.ActiveReason && rule.Spec.For.Duration != 0 {
			rule = v1beta1.PrometheusPatchRuleActive(rule, v1beta1.PendingReason, msg)
			// Await wait time and apply patch or if there is no wait time apply patch right away
		} else if activeCondition.LastTransitionTime.Time.Add(rule.Spec.For.Duration).Before(time.Now()) || rule.Spec.For.Duration == 0 {
			rule = v1beta1.PrometheusPatchRuleActive(rule, v1beta1.ActiveReason, msg)
			rule, err = r.applyPatches(ctx, rule)
		}
	} else {
		msg := "query did not return samples"
		rule = v1beta1.PrometheusPatchRuleNotActive(rule, v1beta1.InactiveReason, msg)
	}

	logger.Info("requeue next reconcile", "interval", rule.Spec.Interval.Duration)

	return rule, ctrl.Result{
		RequeueAfter: rule.Spec.Interval.Duration,
	}, err
}

func (r *PrometheusPatchRuleReconciler) applyPatches(ctx context.Context, rule v1beta1.PrometheusPatchRule) (v1beta1.PrometheusPatchRule, error) {
	if len(rule.Spec.JSON6902Patches) == 0 {
		msg := "no patches have been defined"
		rule = v1beta1.PrometheusPatchRuleNoPatchApplied(rule, v1beta1.NoPatchFoundReason, msg)
		return rule, nil
	}

	for _, v := range rule.Spec.JSON6902Patches {
		var gvr = schema.GroupVersionResource{
			Group:    v.Target.Group,
			Version:  v.Target.Version,
			Resource: v.Target.Resource,
		}

		res := r.DynClient.Resource(gvr).Namespace(v.Target.Namespace)
		var err error

		b, err := json.Marshal(v.Patch)
		if err != nil {
			rule = v1beta1.PrometheusPatchRuleNoPatchApplied(rule, v1beta1.PatchApplyFailedReason, err.Error())
			return rule, err
		}

		if v.Target.Name == "" {
			list, err := res.List(ctx, metav1.ListOptions{
				LabelSelector: v.Target.LabelSelector,
			})

			if err != nil {
				err = fmt.Errorf("failed to find target resources: %w", err)
				rule = v1beta1.PrometheusPatchRuleNoPatchApplied(rule, v1beta1.PatchApplyFailedReason, err.Error())
				return rule, err
			}

			for _, item := range list.Items {
				res := r.DynClient.Resource(gvr).Namespace(item.GetNamespace())
				_, err := res.Patch(ctx, item.GetName(), types.JSONPatchType, b, metav1.PatchOptions{
					FieldManager: r.FieldManager,
				})

				if err != nil {
					break
				}
			}
		} else {
			_, err = res.Patch(ctx, v.Target.Name, types.JSONPatchType, b, metav1.PatchOptions{
				FieldManager: r.FieldManager,
			})
		}

		if err != nil {
			err = fmt.Errorf("failed to apply patch: %w", err)
			rule = v1beta1.PrometheusPatchRuleNoPatchApplied(rule, v1beta1.PatchApplyFailedReason, err.Error())
			return rule, err
		}
	}

	rule = v1beta1.PrometheusPatchRulePatchApplied(rule, v1beta1.PatchAppliedReason)
	return rule, nil
}

func (r *PrometheusPatchRuleReconciler) parseValue(value model.Value) (model.Vector, error) {
	switch value.Type() {
	case model.ValVector:
		return value.(model.Vector), nil
	case model.ValScalar:
		return model.Vector{&model.Sample{
			Value: value.(*model.Scalar).Value,
		}}, nil
	default:
		return nil, errors.New("rule result is not a vector or scalar")
	}
}

func (r *PrometheusPatchRuleReconciler) patchStatus(ctx context.Context, rule *v1beta1.PrometheusPatchRule) error {
	key := client.ObjectKeyFromObject(rule)
	latest := &v1beta1.PrometheusPatchRule{}
	if err := r.Client.Get(ctx, key, latest); err != nil {
		return err
	}

	return r.Client.Status().Patch(ctx, rule, client.MergeFrom(latest))
}
