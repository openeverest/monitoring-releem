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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/managedfields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	common "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

const (
	testNamespace      = "team-a"
	testBindingName    = "orders-releem-5e1f0c2a"
	testBindingUID     = types.UID("6a0e4b8e-3f0c-4d1c-9a51-1f4b0c9e2d11")
	testControllerName = "openeverest.io/monitoring-releem"
	testAgentImage     = "releem/releem-agent:test"
)

func newFakeClient(t *testing.T, objs ...client.Object) client.Client { //nolint:ireturn // the fake is used through the client interface
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(s))
	require.NoError(t, monitoringv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, appsv1.AddToScheme(s))
	return fake.NewClientBuilder().
		WithScheme(s).
		WithTypeConverters(crdTypeConverter(t), managedfields.NewDeducedTypeConverter()).
		WithObjects(objs...).
		WithStatusSubresource(&corev1alpha1.Instance{}, &monitoringv1alpha1.MonitoringClass{},
			&monitoringv1alpha1.MonitoringDestination{}, &monitoringv1alpha1.MonitoringBinding{}, &appsv1.Deployment{}).
		Build()
}

func testController(c client.Client) monitoring.ClassController {
	return monitoring.ClassController{Client: c, FieldOwner: FieldOwner, ControllerName: testControllerName}
}

func testClass() *monitoringv1alpha1.MonitoringClass {
	return &monitoringv1alpha1.MonitoringClass{
		ObjectMeta: metav1.ObjectMeta{Name: "releem", Generation: 1},
		Spec: monitoringv1alpha1.MonitoringClassSpec{
			ControllerName:   testControllerName,
			ExtensionManaged: &monitoringv1alpha1.ExtensionManagedFulfilment{},
			AuthModes:        []common.AuthMode{{Name: AuthModeAPIKey}},
		},
	}
}

func testDestination(parameters string) *monitoringv1alpha1.MonitoringDestination {
	dst := &monitoringv1alpha1.MonitoringDestination{
		ObjectMeta: metav1.ObjectMeta{Name: "releem", Namespace: testNamespace, Generation: 1},
		Spec: monitoringv1alpha1.MonitoringDestinationSpec{
			ClassRef:       common.ObjectRef{Name: "releem"},
			Authentication: common.Authentication{Mode: AuthModeAPIKey, CredentialsSecretRef: &common.SecretRef{Name: "releem-key"}},
		},
	}
	if parameters != "" {
		dst.Spec.Parameters = &runtime.RawExtension{Raw: []byte(parameters)}
	}
	return dst
}

func testBinding(parameters string) *monitoringv1alpha1.MonitoringBinding {
	b := &monitoringv1alpha1.MonitoringBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: testBindingName, Namespace: testNamespace, UID: testBindingUID, Generation: 1,
			Labels: monitoring.BindingLabels("orders", "releem", "releem", "releem"),
		},
		Spec: monitoringv1alpha1.MonitoringBindingSpec{
			InstanceRef:    common.ObjectRef{Name: "orders"},
			DestinationRef: common.MonitoringDestinationRef{Kind: "MonitoringDestination", Name: "releem"},
			ClassRef:       &common.ObjectRef{Name: "releem"},
			ExecutionMode:  corev1alpha1.MonitoringExecutionModeExtensionManaged,
		},
	}
	if parameters != "" {
		b.Spec.Parameters = &runtime.RawExtension{Raw: []byte(parameters)}
	}
	b.Status.Conditions = []metav1.Condition{{
		Type: monitoringv1alpha1.MonitoringBindingConditionAccepted, Status: metav1.ConditionTrue, Reason: monitoringv1alpha1.ReasonAccepted,
		ObservedGeneration: 1, LastTransitionTime: metav1.Now(),
	}}
	return b
}

func engineSelector() map[string]string {
	return map[string]string{monitoringv1alpha1.BindingInstanceLabel: "orders", "core.openeverest.io/component": "engine"}
}

func connection(kind string, tls bool) corev1alpha1.ConnectionEndpoint {
	ep := corev1alpha1.ConnectionEndpoint{
		Component: "engine", Kind: kind, PodSelector: engineSelector(), Container: "db",
		Port: intstr.FromString("sql"),
		Auth: &corev1alpha1.MetricsEndpointAuth{CredentialProfile: corev1alpha1.CredentialProfileMonitor},
	}
	if tls {
		ep.TLS = &corev1alpha1.ConnectionTLS{Required: true}
	}
	return ep
}

func testInstance(connections ...corev1alpha1.ConnectionEndpoint) *corev1alpha1.Instance {
	in := &corev1alpha1.Instance{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}}
	in.Status.Monitoring = &corev1alpha1.InstanceMonitoringStatus{Sources: &corev1alpha1.MonitoringSources{
		Credentials: []corev1alpha1.MonitoringCredential{{
			Profile: corev1alpha1.CredentialProfileMonitor, SecretRef: common.SecretRef{Name: "orders-monitoring-monitor"},
		}},
		Connections: connections,
	}}
	return in
}

func dbPod(name, ip string, port int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: engineSelector()},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "sidecar", Ports: []corev1.ContainerPort{{Name: "sql", ContainerPort: 1}}},
			{
				Name:      "db",
				Ports:     []corev1.ContainerPort{{Name: "sql", ContainerPort: port}},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}},
			},
		}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

func reconcileBinding(t *testing.T, c client.Client) ctrl.Result {
	t.Helper()
	r := &bindingReconciler{client: c, cc: testController(c), agent: agentOptions{Image: testAgentImage, Resources: defaultAgentResources()}}
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: testBindingName}})
	require.NoError(t, err)
	return res
}

func configured(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()
	b := &monitoringv1alpha1.MonitoringBinding{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: testBindingName}, b))
	return meta.FindStatusCondition(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionConfigured)
}

func liveAgents(t *testing.T, c client.Client) map[string]appsv1.Deployment {
	t.Helper()
	list := &appsv1.DeploymentList{}
	require.NoError(t, c.List(t.Context(), list, client.InNamespace(testNamespace)))
	out := map[string]appsv1.Deployment{}
	for _, d := range list.Items {
		out[d.Name] = d
	}
	return out
}

func envOf(d appsv1.Deployment) map[string]corev1.EnvVar {
	out := map[string]corev1.EnvVar{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		out[e.Name] = e
	}
	return out
}

func secretRef(name, key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}}
}

func TestReconcileRunsOneAgentPerPostgreSQLPod(t *testing.T) {
	t.Parallel()
	notRunning := dbPod("orders-3", "", 5432)
	notRunning.Status.Phase = corev1.PodPending
	c := newFakeClient(t, testClass(), testDestination(`{"region":"EU","hostnamePrefix":"prod-"}`),
		testInstance(connection("postgresql", true)), testBinding(`{"queryOptimization":true}`),
		dbPod("orders-1", "10.0.0.1", 5432), dbPod("orders-2", "10.0.0.2", 5432), notRunning)

	res := reconcileBinding(t, c)

	agents := liveAgents(t, c)
	require.Len(t, agents, 2, "no agent for a pod without an address")
	agent := agents[testBindingName+"-orders-1"]
	assert.True(t, metav1.IsControlledBy(&agent, testBinding("")), "removing the entry garbage-collects the agents")
	assert.Equal(t, appsv1.RecreateDeploymentStrategyType, agent.Spec.Strategy.Type)
	assert.Equal(t, testAgentImage, agent.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, managedByValue, agent.Labels[ManagedByLabel])
	env := envOf(agent)
	assert.Equal(t, secretRef("releem-key", "apiKey"), env["RELEEM_API_KEY"].ValueFrom)
	assert.Equal(t, secretRef("orders-monitoring-monitor", "username"), env["RELEEM_PG_LOGIN"].ValueFrom)
	assert.Equal(t, secretRef("orders-monitoring-monitor", "password"), env["RELEEM_PG_PASSWORD"].ValueFrom)
	assert.Equal(t, "10.0.0.1", env["RELEEM_PG_HOST"].Value)
	assert.Equal(t, "5432", env["RELEEM_PG_PORT"].Value, "the named port of the endpoint's container")
	assert.Equal(t, "true", env["RELEEM_PG_SSL_MODE"].Value)
	assert.Equal(t, "2048", env["MEMORY_LIMIT"].Value)
	assert.Equal(t, "prod-orders-1."+testNamespace, env["RELEEM_HOSTNAME"].Value)
	assert.Equal(t, "EU", env["RELEEM_REGION"].Value)
	assert.Equal(t, "true", env["RELEEM_QUERY_OPTIMIZATION"].Value)
	assert.NotContains(t, env, "DB_HOST")
	probe := agent.Spec.Template.Spec.Containers[0].LivenessProbe
	require.NotNil(t, probe, "a rotated password restarts the agent")
	assert.Contains(t, probe.Exec.Command[2], `-U "$RELEEM_PG_LOGIN"`)

	cond := configured(t, c)
	assert.Equal(t, string(monitoringv1alpha1.ReasonAgentNotReady), cond.Reason, "agents are not available yet")
	assert.Equal(t, agentStartingRecheck, res.RequeueAfter)

	for name := range agents {
		d := agents[name]
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, AvailableReplicas: 1, UpdatedReplicas: 1}
		require.NoError(t, c.Status().Update(t.Context(), &d))
	}
	res = reconcileBinding(t, c)
	cond = configured(t, c)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Releem agents "+testBindingName+"-orders-1, "+testBindingName+"-orders-2", cond.Message)
	assert.Zero(t, res.RequeueAfter)
}

func TestReconcileRendersMySQLAgents(t *testing.T) {
	t.Parallel()
	c := newFakeClient(t, testClass(), testDestination(""), testInstance(connection("mariadb", false)), testBinding(""),
		dbPod("orders-1", "10.0.0.1", 3306))

	reconcileBinding(t, c)

	env := envOf(liveAgents(t, c)[testBindingName+"-orders-1"])
	assert.Equal(t, secretRef("orders-monitoring-monitor", "username"), env["DB_USER"].ValueFrom)
	assert.Equal(t, "10.0.0.1", env["DB_HOST"].Value)
	assert.Equal(t, "3306", env["DB_PORT"].Value)
	assert.Equal(t, "false", env["DB_SSL"].Value)
	assert.Equal(t, "false", env["RELEEM_QUERY_OPTIMIZATION"].Value)
	assert.NotContains(t, env, "RELEEM_REGION", "US is the agent's default")
	assert.NotContains(t, env, "RELEEM_PG_HOST")
	probe := liveAgents(t, c)[testBindingName+"-orders-1"].Spec.Template.Spec.Containers[0].LivenessProbe
	require.NotNil(t, probe)
	assert.Contains(t, probe.Exec.Command[2], `mariadb --connect-timeout=5 -h "$DB_HOST"`)
}

func TestReconcileFollowsThePods(t *testing.T) {
	t.Parallel()
	c := newFakeClient(t, testClass(), testDestination(""), testInstance(connection("postgresql", false)), testBinding(""),
		dbPod("orders-1", "10.0.0.1", 5432), dbPod("orders-2", "10.0.0.2", 5432))
	reconcileBinding(t, c)
	require.Len(t, liveAgents(t, c), 2)

	moved := &corev1.Pod{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "orders-1"}, moved))
	moved.Status.PodIP = "10.0.0.9"
	require.NoError(t, c.Status().Update(t.Context(), moved))
	require.NoError(t, c.Delete(t.Context(), dbPod("orders-2", "", 0)))
	reconcileBinding(t, c)

	agents := liveAgents(t, c)
	require.Len(t, agents, 1, "the agent of a pod that is gone is deleted")
	assert.Equal(t, "10.0.0.9", envOf(agents[testBindingName+"-orders-1"])["RELEEM_PG_HOST"].Value)
}

func TestReconcileReportsWhyNothingRuns(t *testing.T) {
	t.Parallel()
	noAuth := connection("postgresql", false)
	noAuth.Auth = nil
	anonymous := testDestination("")
	anonymous.Spec.Authentication = common.Authentication{Mode: common.AuthModeAnonymous}
	tests := []struct {
		name        string
		destination *monitoringv1alpha1.MonitoringDestination
		instance    *corev1alpha1.Instance
		wantReason  monitoringv1alpha1.ConfiguredReason
		wantMessage string
	}{
		{
			name: "no connection endpoints", destination: testDestination(""), instance: testInstance(),
			wantReason: monitoringv1alpha1.ReasonEndpointNotServing, wantMessage: "publishes no connection endpoints",
		},
		{
			name: "unsupported kind", destination: testDestination(""), instance: testInstance(connection("mongodb", false)),
			wantReason: monitoringv1alpha1.ReasonRenderFailed, wantMessage: `kind "mongodb" is not monitored by Releem`,
		},
		{
			name: "no credential", destination: testDestination(""), instance: testInstance(noAuth),
			wantReason: monitoringv1alpha1.ReasonRenderFailed, wantMessage: "publishes no credential profile",
		},
		{
			name: "no API key", destination: anonymous, instance: testInstance(connection("postgresql", false)),
			wantReason: monitoringv1alpha1.ReasonRenderFailed, wantMessage: "authentication mode APIKey",
		},
		{
			name: "invalid region", destination: testDestination(`{"region":"APAC"}`), instance: testInstance(connection("postgresql", false)),
			wantReason: monitoringv1alpha1.ReasonRenderFailed, wantMessage: `region "APAC"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newFakeClient(t, testClass(), tt.destination, tt.instance, testBinding(""), dbPod("orders-1", "10.0.0.1", 5432))
			reconcileBinding(t, c)
			cond := configured(t, c)
			assert.Equal(t, string(tt.wantReason), cond.Reason)
			assert.Contains(t, cond.Message, tt.wantMessage)
			assert.Empty(t, liveAgents(t, c))
		})
	}
}

func TestAgentNameFitsPodNames(t *testing.T) {
	t.Parallel()
	long := agentName(strings.Repeat("b", 60), "orders-1")
	assert.LessOrEqual(t, len(long), maxAgentName)
	assert.NotEqual(t, long, agentName(strings.Repeat("b", 60), "orders-2"))
	assert.Equal(t, "b-p", agentName("b", "p"))
}

func TestReconcileWaitsForANodeToBeReadyButKeepsRunningAgents(t *testing.T) {
	t.Parallel()
	joining := dbPod("orders-2", "10.0.0.2", 5432)
	joining.Status.Conditions[0].Status = corev1.ConditionFalse
	c := newFakeClient(t, testClass(), testDestination(""), testInstance(connection("mysql", false)), testBinding(""),
		dbPod("orders-1", "10.0.0.1", 3306), joining)

	reconcileBinding(t, c)
	agents := liveAgents(t, c)
	require.Len(t, agents, 1, "a node still joining the cluster refuses connections")
	assert.Contains(t, agents, testBindingName+"-orders-1")

	rejoining := &corev1.Pod{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "orders-1"}, rejoining))
	rejoining.Status.Conditions[0].Status = corev1.ConditionFalse
	rejoining.Status.PodIP = "10.0.0.9"
	require.NoError(t, c.Status().Update(t.Context(), rejoining))
	reconcileBinding(t, c)
	held := liveAgents(t, c)[testBindingName+"-orders-1"]
	assert.Equal(t, "10.0.0.1", envOf(held)["DB_HOST"].Value, "the agent of a rejoining node is left as it is")

	rejoining.Status.Conditions[0].Status = corev1.ConditionTrue
	require.NoError(t, c.Status().Update(t.Context(), rejoining))
	reconcileBinding(t, c)
	assert.Equal(t, "10.0.0.9", envOf(liveAgents(t, c)[testBindingName+"-orders-1"])["DB_HOST"].Value, "and follows it once it is ready")
}
