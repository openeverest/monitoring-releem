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

// Package controller reconciles ExtensionManaged MonitoringBindings of a
// releem MonitoringClass into one releem-agent Deployment per database pod
// of every bound Instance.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

// FieldOwner is the server-side-apply field manager of everything this
// controller writes.
const FieldOwner = "openeverest.io/monitoring-releem"

// applyRetryInterval paces retries while agents cannot be written.
const applyRetryInterval = 30 * time.Second

// Options configures the controllers.
type Options struct {
	// ControllerName claims the MonitoringClasses whose controllerName it is.
	ControllerName string
	// AgentImage is the releem-agent image.
	AgentImage string
}

// CacheOptions limits the manager's caches to the pods of OpenEverest
// Instances and the agents this controller runs.
func CacheOptions() (cache.Options, error) {
	instancePods, err := labels.NewRequirement(monitoringv1alpha1.BindingInstanceLabel, selection.Exists, nil)
	if err != nil {
		return cache.Options{}, fmt.Errorf("pod selector: %w", err)
	}
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Pod{}:        {Label: labels.NewSelector().Add(*instancePods)},
		&appsv1.Deployment{}: {Label: labels.SelectorFromSet(labels.Set{ManagedByLabel: managedByValue})},
	}}, nil
}

// Setup registers the controllers serving every MonitoringClass whose
// controllerName is opts.ControllerName: one claims the classes, one
// reports destination readiness and one runs the agents of every binding.
func Setup(mgr ctrl.Manager, opts Options) error {
	cc := monitoring.ClassController{Client: mgr.GetClient(), FieldOwner: FieldOwner, ControllerName: opts.ControllerName}

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-releem-class").
		For(&monitoringv1alpha1.MonitoringClass{}, builder.WithPredicates(
			predicate.GenerationChangedPredicate{},
			predicate.NewPredicateFuncs(func(obj client.Object) bool {
				class, ok := obj.(*monitoringv1alpha1.MonitoringClass)
				return ok && cc.OwnsClass(class)
			}),
		)).
		Complete(&classReconciler{client: mgr.GetClient(), cc: cc}); err != nil {
		return fmt.Errorf("set up MonitoringClass controller: %w", err)
	}

	dr := &destinationReconciler{client: mgr.GetClient(), cc: cc}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-releem-destination").
		For(&monitoringv1alpha1.MonitoringDestination{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&monitoringv1alpha1.MonitoringClass{}, handler.EnqueueRequestsFromMapFunc(dr.destinationsOfClass),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(dr); err != nil {
		return fmt.Errorf("set up MonitoringDestination controller: %w", err)
	}

	agent := agentOptions{Image: opts.AgentImage, Resources: defaultAgentResources()}
	r := &bindingReconciler{client: mgr.GetClient(), cc: cc, agent: agent}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-releem").
		For(&monitoringv1alpha1.MonitoringBinding{}, builder.WithPredicates(monitoring.ExtensionManagedBindingPredicate())).
		Owns(&appsv1.Deployment{}).
		Watches(&corev1alpha1.Instance{}, handler.EnqueueRequestsFromMapFunc(r.bindingsForInstance),
			builder.WithPredicates(sourcesChanged())).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.bindingsForPod),
			builder.WithPredicates(podAddressChanged())).
		Watches(&monitoringv1alpha1.MonitoringDestination{}, handler.EnqueueRequestsFromMapFunc(r.bindingsForDestination),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r); err != nil {
		return fmt.Errorf("set up MonitoringBinding controller: %w", err)
	}
	return nil
}

// classReconciler claims the served classes and leaves any other unclaimed.
type classReconciler struct {
	client client.Client
	cc     monitoring.ClassController
}

func (r *classReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	class := &monitoringv1alpha1.MonitoringClass{}
	if err := r.client.Get(ctx, req.NamespacedName, class); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, r.cc.ClaimClass(ctx, class)
}

// destinationReconciler reports Ready on destinations of our classes once
// their parameters render. Releem has no endpoint to check an API key
// against, and the controller does not read the key: the agents report a
// rejected key in their logs only.
type destinationReconciler struct {
	client client.Client
	cc     monitoring.ClassController
}

func (r *destinationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	dst := &monitoringv1alpha1.MonitoringDestination{}
	if err := r.client.Get(ctx, req.NamespacedName, dst); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	owned, err := r.cc.OwnsClassNamed(ctx, dst.Spec.ClassRef.Name)
	if err != nil || !owned {
		return ctrl.Result{}, err
	}
	params, err := decodeDestinationParameters(dst.Spec.Parameters)
	if err != nil {
		return ctrl.Result{}, r.cc.SetDestinationReady(ctx, dst, monitoringv1alpha1.ReasonReadyInvalidParameters, err.Error(), "")
	}
	return ctrl.Result{}, r.cc.SetDestinationReady(ctx, dst, monitoringv1alpha1.ReasonReady,
		fmt.Sprintf("agents report to the Releem %s region; the API key is checked by the agents", params.Region), "")
}

// destinationsOfClass enqueues the destinations of a class this controller
// claims, so they are checked once the class appears or changes.
func (r *destinationReconciler) destinationsOfClass(ctx context.Context, obj client.Object) []reconcile.Request {
	class, ok := obj.(*monitoringv1alpha1.MonitoringClass)
	if !ok || !r.cc.OwnsClass(class) {
		return nil
	}
	list := &monitoringv1alpha1.MonitoringDestinationList{}
	if err := r.client.List(ctx, list); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing MonitoringDestinations failed", "class", class.Name)
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.ClassRef.Name == class.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

// sourcesChanged passes Instance updates that change
// status.monitoring.sources, the only Instance field agents derive from.
func sourcesChanged() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldIn, okOld := e.ObjectOld.(*corev1alpha1.Instance)
			newIn, okNew := e.ObjectNew.(*corev1alpha1.Instance)
			return okOld && okNew && !equality.Semantic.DeepEqual(sourcesOf(oldIn), sourcesOf(newIn))
		},
	}
}

// podAddressChanged passes pods appearing, going and changing the address,
// phase or readiness agents depend on.
func podAddressChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, okOld := e.ObjectOld.(*corev1.Pod)
			newPod, okNew := e.ObjectNew.(*corev1.Pod)
			return okOld && okNew && (oldPod.Status.PodIP != newPod.Status.PodIP ||
				oldPod.Status.Phase != newPod.Status.Phase ||
				podReady(oldPod) != podReady(newPod) ||
				oldPod.DeletionTimestamp.IsZero() != newPod.DeletionTimestamp.IsZero())
		},
	}
}

func (r *bindingReconciler) bindingsForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	in, ok := obj.(*corev1alpha1.Instance)
	if !ok {
		return nil
	}
	return r.bindingsOfInstance(ctx, in.Namespace, in.Name)
}

func (r *bindingReconciler) bindingsForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	instance := obj.GetLabels()[monitoringv1alpha1.BindingInstanceLabel]
	if instance == "" {
		return nil
	}
	return r.bindingsOfInstance(ctx, obj.GetNamespace(), instance)
}

func (r *bindingReconciler) bindingsOfInstance(ctx context.Context, namespace, instance string) []reconcile.Request {
	list := &monitoringv1alpha1.MonitoringBindingList{}
	if err := r.client.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{
		monitoringv1alpha1.BindingInstanceLabel: instance,
	}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing bindings for Instance failed", "instance", instance)
		return nil
	}
	return r.served(ctx, list.Items)
}

func (r *bindingReconciler) bindingsForDestination(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &monitoringv1alpha1.MonitoringBindingList{}
	if err := r.client.List(ctx, list, client.InNamespace(obj.GetNamespace()), client.MatchingLabels{
		monitoringv1alpha1.BindingDestinationLabel: obj.GetName(),
	}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing bindings for MonitoringDestination failed", "destination", obj.GetName())
		return nil
	}
	return r.served(ctx, list.Items)
}

// served returns a request for each binding this controller executes.
func (r *bindingReconciler) served(ctx context.Context, bindings []monitoringv1alpha1.MonitoringBinding) []reconcile.Request {
	var reqs []reconcile.Request
	for i := range bindings {
		executes, err := r.cc.OwnsBinding(ctx, &bindings[i])
		if err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "resolving the class of a binding failed", "binding", bindings[i].Name)
			continue
		}
		if executes {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&bindings[i])})
		}
	}
	return reqs
}

func toUnstructured(obj any) (*unstructured.Unstructured, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(data); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return u, nil
}
