// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

// agentStartingRecheck paces the checks of agents that are not available yet.
const agentStartingRecheck = 15 * time.Second

// bindingReconciler runs one releem-agent per database pod of a binding.
type bindingReconciler struct {
	client client.Client
	cc     monitoring.ClassController
	agent  agentOptions
}

func (r *bindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	b := &monitoringv1alpha1.MonitoringBinding{}
	if err := r.client.Get(ctx, req.NamespacedName, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !b.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if executes, err := r.cc.OwnsBinding(ctx, b); err != nil || !executes {
		return ctrl.Result{}, err
	}

	var res renderResult
	var err error
	if monitoring.IsAccepted(b) {
		res, err = r.render(ctx, b)
	} else {
		res.reason, res.message, err = r.notAcceptedReason(ctx, b)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: res.requeue}, r.cc.SetConfigured(ctx, b, res.reason, res.message)
}

type renderResult struct {
	reason  monitoringv1alpha1.ConfiguredReason
	message string
	requeue time.Duration
}

func bindingClass(b *monitoringv1alpha1.MonitoringBinding) string {
	return b.Labels[monitoringv1alpha1.BindingClassLabel]
}

// render brings the binding's agents in line with the Instance's connection
// endpoints and the pods they select. Inputs that cannot be rendered keep
// the live agents.
func (r *bindingReconciler) render(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding) (renderResult, error) {
	in, dst, err := r.cc.Resolve(ctx, b)
	if apierrors.IsNotFound(err) {
		return renderResult{reason: monitoringv1alpha1.ReasonRenderFailed, message: err.Error()}, nil
	}
	if err != nil {
		return renderResult{}, err
	}
	dstParams, err := decodeDestinationParameters(dst.Spec.Parameters)
	if err != nil {
		return failed(fmt.Sprintf("MonitoringDestination %s: %v", dst.Name, err)), nil
	}
	instParams, err := decodeInstanceParameters(b.Spec.Parameters)
	if err != nil {
		return failed(err.Error()), nil
	}
	sources := sourcesOf(in)
	if sources == nil || len(sources.Connections) == 0 {
		return renderResult{
			reason:  monitoringv1alpha1.ReasonEndpointNotServing,
			message: fmt.Sprintf("Instance %s publishes no connection endpoints yet", in.Name),
		}, r.pruneStale(ctx, b, nil)
	}

	agents, res, err := r.renderAgents(ctx, agentInput{binding: b, destination: dst, dstParams: dstParams, instParams: instParams, sources: sources})
	if err != nil || res != nil {
		return deref(res), err
	}

	names := make([]string, 0, len(agents))
	for _, agent := range agents {
		u, err := toUnstructured(agent)
		if err != nil {
			return renderResult{}, err
		}
		if err := r.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
			return renderResult{
				reason:  monitoringv1alpha1.ReasonApplyFailed,
				message: fmt.Sprintf("apply Deployment %s: %v; retrying in %s", agent.Name, err, applyRetryInterval),
				requeue: applyRetryInterval,
			}, nil
		}
		names = append(names, agent.Name)
	}
	if err := r.pruneStale(ctx, b, names); err != nil {
		return renderResult{}, err
	}
	slices.Sort(names)
	if len(names) == 0 {
		return renderResult{
			reason:  monitoringv1alpha1.ReasonEndpointNotServing,
			message: fmt.Sprintf("no running database pod of Instance %s yet", in.Name),
			requeue: agentStartingRecheck,
		}, nil
	}
	return r.agentsReadiness(ctx, b, names)
}

// renderAgents renders one agent per running pod of every connection
// endpoint; a non-nil result reports why they cannot be rendered.
func (r *bindingReconciler) renderAgents(ctx context.Context, base agentInput) ([]*appsv1.Deployment, *renderResult, error) {
	b := base.binding
	var agents []*appsv1.Deployment
	for _, ep := range base.sources.Connections {
		if ep.PodSelector[monitoringv1alpha1.BindingInstanceLabel] != b.Spec.InstanceRef.Name {
			res := failed(fmt.Sprintf("component %q: pod selector does not select %s=%s",
				ep.Component, monitoringv1alpha1.BindingInstanceLabel, b.Spec.InstanceRef.Name))
			return nil, &res, nil
		}
		pods, err := r.servingPods(ctx, b.Namespace, ep.PodSelector)
		if err != nil {
			return nil, nil, err
		}
		for i := range pods {
			in := base
			in.endpoint, in.pod = ep, &pods[i]
			agent, err := agentFor(in, r.agent)
			if err != nil {
				res := failed(fmt.Sprintf("Instance %s: %v", b.Spec.InstanceRef.Name, err))
				return nil, &res, nil
			}
			agents = append(agents, agent)
		}
	}
	return agents, nil, nil
}

func deref(res *renderResult) renderResult {
	if res == nil {
		return renderResult{}
	}
	return *res
}

func failed(message string) renderResult {
	return renderResult{reason: monitoringv1alpha1.ReasonRenderFailed, message: message}
}

// servingPods are the running pods selector matches; an agent needs the
// pod's address.
func (r *bindingReconciler) servingPods(ctx context.Context, namespace string, selector map[string]string) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := r.client.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels(selector)); err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	pods := slices.DeleteFunc(list.Items, func(p corev1.Pod) bool {
		return !p.DeletionTimestamp.IsZero() || p.Status.Phase != corev1.PodRunning || p.Status.PodIP == ""
	})
	slices.SortFunc(pods, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	return pods, nil
}

// agentsReadiness reports Configured once every agent runs.
func (r *bindingReconciler) agentsReadiness(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding, names []string) (renderResult, error) {
	var waiting []string
	for _, name := range names {
		d := &appsv1.Deployment{}
		if err := r.client.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, d); err != nil {
			if apierrors.IsNotFound(err) {
				waiting = append(waiting, name)
				continue
			}
			return renderResult{}, fmt.Errorf("get Deployment %s: %w", name, err)
		}
		if d.Status.ObservedGeneration < d.Generation || d.Status.AvailableReplicas < 1 || d.Status.UpdatedReplicas < 1 {
			waiting = append(waiting, name)
		}
	}
	if len(waiting) > 0 {
		return renderResult{
			reason:  monitoringv1alpha1.ReasonAgentNotReady,
			message: fmt.Sprintf("%d of %d Releem agents are not running yet: %s", len(waiting), len(names), strings.Join(waiting, ", ")),
			requeue: agentStartingRecheck,
		}, nil
	}
	return renderResult{reason: monitoringv1alpha1.ReasonConfigured, message: "Releem agents " + strings.Join(names, ", ")}, nil
}

// notAcceptedReason picks the Configured reason of a binding core has not
// accepted: Retained while it still owns agents, NotAccepted when core
// rejected it, Pending while core has not decided.
func (r *bindingReconciler) notAcceptedReason(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding) (monitoringv1alpha1.ConfiguredReason, string, error) {
	accepted := meta.FindStatusCondition(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionAccepted)
	decided := accepted != nil && accepted.ObservedGeneration == b.Generation
	acceptedReason := "not evaluated yet"
	if decided {
		acceptedReason = accepted.Reason
		if accepted.Message != "" {
			acceptedReason += ": " + accepted.Message
		}
	}
	live, err := r.ownedAgents(ctx, b)
	if err != nil {
		return "", "", err
	}
	switch {
	case len(live) > 0:
		return monitoringv1alpha1.ReasonRetained, "keeping the Releem agents while the binding is not accepted (" + acceptedReason + ")", nil
	case decided && accepted.Status == metav1.ConditionFalse:
		return monitoringv1alpha1.ReasonNotAccepted, "binding not accepted: " + acceptedReason, nil
	default:
		return monitoringv1alpha1.ReasonPending, "waiting for the binding to be accepted", nil
	}
}

// ownedAgents lists the agent Deployments the binding controls.
func (r *bindingReconciler) ownedAgents(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding) ([]appsv1.Deployment, error) {
	list := &appsv1.DeploymentList{}
	if err := r.client.List(ctx, list, client.InNamespace(b.Namespace), client.MatchingLabels{
		ManagedByLabel:                          managedByValue,
		monitoringv1alpha1.BindingInstanceLabel: b.Spec.InstanceRef.Name,
	}); err != nil {
		return nil, fmt.Errorf("list Deployments: %w", err)
	}
	return slices.DeleteFunc(list.Items, func(d appsv1.Deployment) bool { return !metav1.IsControlledBy(&d, b) }), nil
}

// pruneStale deletes the binding's agents for pods that are gone.
func (r *bindingReconciler) pruneStale(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding, keep []string) error {
	live, err := r.ownedAgents(ctx, b)
	if err != nil {
		return err
	}
	for i := range live {
		if slices.Contains(keep, live[i].Name) {
			continue
		}
		if err := r.client.Delete(ctx, &live[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete stale Deployment %s: %w", live[i].Name, err)
		}
	}
	return nil
}

func sourcesOf(in *corev1alpha1.Instance) *corev1alpha1.MonitoringSources {
	if in.Status.Monitoring == nil {
		return nil
	}
	return in.Status.Monitoring.Sources
}
