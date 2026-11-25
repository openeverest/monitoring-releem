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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

const (
	// ManagedByLabel marks the agent Deployments this controller owns; the
	// manager caches only those.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "monitoring-releem"
	// agentPodLabel names the database pod an agent reports on.
	agentPodLabel = "monitoring.openeverest.io/pod"

	agentContainerName = "releem-agent"
	// maxAgentName keeps the Deployment's pod names, which add two hashes,
	// within the 63 characters of a DNS label.
	maxAgentName = 47
)

// engineFamily is the database protocol an agent speaks, picked from the
// endpoint's kind.
type engineFamily string

const (
	familyMySQL      engineFamily = "mysql"
	familyPostgreSQL engineFamily = "postgresql"
)

// supportedKinds are the component kinds Releem monitors, and the family of
// each.
var supportedKinds = map[string]engineFamily{ //nolint:gochecknoglobals // constant table
	"mysql":      familyMySQL,
	"mariadb":    familyMySQL,
	"postgresql": familyPostgreSQL,
}

// agentOptions are the admin-level settings of every agent, set on the
// controller command line.
type agentOptions struct {
	Image     string
	Resources corev1.ResourceRequirements
}

// agentInput is everything one agent is rendered from.
type agentInput struct {
	binding     *monitoringv1alpha1.MonitoringBinding
	destination *monitoringv1alpha1.MonitoringDestination
	dstParams   destinationParameters
	instParams  instanceParameters
	sources     *corev1alpha1.MonitoringSources
	endpoint    corev1alpha1.ConnectionEndpoint
	pod         *corev1.Pod
}

// agentName is the Deployment of the agent reporting on pod for binding.
func agentName(binding, pod string) string {
	name := binding + "-" + pod
	if len(name) <= maxAgentName {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:maxAgentName-9] + "-" + hex.EncodeToString(sum[:])[:8]
}

// agentFor renders the releem-agent Deployment that reports on one database
// pod. The agent connects to the pod directly, so Releem sees each server
// of the Instance separately, and gets its database login and API key as
// secretKeyRef environment variables: the controller reads neither.
func agentFor(in agentInput, opts agentOptions) (*appsv1.Deployment, error) {
	family, ok := supportedKinds[in.endpoint.Kind]
	if !ok {
		return nil, fmt.Errorf("component %q: kind %q is not monitored by Releem", in.endpoint.Component, in.endpoint.Kind)
	}
	if in.pod.Status.PodIP == "" {
		return nil, fmt.Errorf("pod %s has no IP yet", in.pod.Name)
	}
	port, err := resolvePort(in.pod, in.endpoint)
	if err != nil {
		return nil, err
	}
	login, err := loginSecret(in.sources, in.endpoint)
	if err != nil {
		return nil, err
	}
	apiKey, err := apiKeySecret(in.destination)
	if err != nil {
		return nil, err
	}
	env := agentEnv(in, family, port, login, apiKey)

	name := agentName(in.binding.Name, in.pod.Name)
	labels := map[string]string{
		ManagedByLabel:                             managedByValue,
		monitoringv1alpha1.BindingInstanceLabel:    in.binding.Spec.InstanceRef.Name,
		monitoringv1alpha1.BindingClassLabel:       bindingClass(in.binding),
		monitoringv1alpha1.BindingDestinationLabel: in.binding.Spec.DestinationRef.Name,
		agentPodLabel:                              in.pod.Name,
	}
	selector := map[string]string{"app.kubernetes.io/name": agentContainerName, "app.kubernetes.io/instance": name}
	podLabels := map[string]string{ManagedByLabel: managedByValue}
	maps.Copy(podLabels, selector)
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       in.binding.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{monitoring.OwnedBy(in.binding)},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			// Two agents must never report the same server at once.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: new(false),
					EnableServiceLinks:           new(false),
					Containers: []corev1.Container{{
						Name:      agentContainerName,
						Image:     opts.Image,
						Env:       env,
						Resources: opts.Resources,
					}},
				},
			},
		},
	}, nil
}

// agentEnv configures the agent: API key and database login by Secret
// reference, the database pod's address and the destination's settings.
func agentEnv(in agentInput, family engineFamily, port int32, login, apiKeySecret string) []corev1.EnvVar {
	env := []corev1.EnvVar{
		secretEnv("RELEEM_API_KEY", apiKeySecret, CredentialAPIKey),
		{Name: "RELEEM_HOSTNAME", Value: in.dstParams.HostnamePrefix + in.pod.Name + "." + in.pod.Namespace},
		{Name: "MEMORY_LIMIT", Value: strconv.FormatInt(memoryLimitMiB(in.pod, in.endpoint.Container), 10)},
		{Name: "INSTANCE_TYPE", Value: "local"},
		{Name: "RELEEM_QUERY_OPTIMIZATION", Value: strconv.FormatBool(in.instParams.QueryOptimization)},
	}
	if in.dstParams.Region == RegionEU {
		env = append(env, corev1.EnvVar{Name: "RELEEM_REGION", Value: RegionEU})
	}
	// The agent encrypts without verifying the server (sslmode=require,
	// tls=skip-verify), so the published CA is not used.
	useTLS := strconv.FormatBool(in.endpoint.TLS != nil)
	host, portValue := in.pod.Status.PodIP, strconv.Itoa(int(port))
	switch family {
	case familyPostgreSQL:
		env = append(env,
			secretEnv("RELEEM_PG_LOGIN", login, corev1alpha1.CredentialSecretKeyUsername),
			secretEnv("RELEEM_PG_PASSWORD", login, corev1alpha1.CredentialSecretKeyPassword),
			corev1.EnvVar{Name: "RELEEM_PG_HOST", Value: host},
			corev1.EnvVar{Name: "RELEEM_PG_PORT", Value: portValue},
			corev1.EnvVar{Name: "RELEEM_PG_SSL_MODE", Value: useTLS},
		)
	case familyMySQL:
		env = append(env,
			secretEnv("DB_USER", login, corev1alpha1.CredentialSecretKeyUsername),
			secretEnv("DB_PASSWORD", login, corev1alpha1.CredentialSecretKeyPassword),
			corev1.EnvVar{Name: "DB_HOST", Value: host},
			corev1.EnvVar{Name: "DB_PORT", Value: portValue},
			corev1.EnvVar{Name: "DB_SSL", Value: useTLS},
		)
	}

	return env
}

// apiKeySecret is the destination's credentials Secret holding the API key.
func apiKeySecret(dst *monitoringv1alpha1.MonitoringDestination) (string, error) {
	ref := dst.Spec.Authentication.CredentialsSecretRef
	if dst.Spec.Authentication.Mode != AuthModeAPIKey || ref == nil {
		return "", fmt.Errorf("MonitoringDestination %s: authentication mode %s with a credentials Secret is required",
			dst.Name, AuthModeAPIKey)
	}
	return ref.Name, nil
}

// resolvePort is the endpoint's container port number on pod.
func resolvePort(pod *corev1.Pod, ep corev1alpha1.ConnectionEndpoint) (int32, error) {
	if ep.Port.Type == intstr.Int {
		if ep.Port.IntVal <= 0 {
			return 0, errors.New("connection endpoint has no port")
		}
		return ep.Port.IntVal, nil
	}
	for _, c := range pod.Spec.Containers {
		if ep.Container != "" && c.Name != ep.Container {
			continue
		}
		for _, p := range c.Ports {
			if p.Name == ep.Port.StrVal {
				return p.ContainerPort, nil
			}
		}
	}
	return 0, fmt.Errorf("pod %s has no container port %q", pod.Name, ep.Port.StrVal)
}

// loginSecret is the Secret of the credential profile the endpoint names.
func loginSecret(sources *corev1alpha1.MonitoringSources, ep corev1alpha1.ConnectionEndpoint) (string, error) {
	if ep.Auth == nil {
		return "", fmt.Errorf("component %q publishes no credential profile to log in with", ep.Component)
	}
	i := slices.IndexFunc(sources.Credentials, func(c corev1alpha1.MonitoringCredential) bool {
		return c.Profile == ep.Auth.CredentialProfile
	})
	if i < 0 {
		return "", fmt.Errorf("credential profile %q is not published by the Instance", ep.Auth.CredentialProfile)
	}
	return sources.Credentials[i].SecretRef.Name, nil
}

// memoryLimitMiB is the memory limit of the database container, which
// Releem sizes its recommendations to; 0 when it has none.
func memoryLimitMiB(pod *corev1.Pod, container string) int64 {
	for _, c := range pod.Spec.Containers {
		if container != "" && c.Name != container {
			continue
		}
		if limit, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			return limit.Value() / (1 << 20)
		}
	}
	return 0
}

func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret},
		Key:                  key,
	}}}
}

// defaultAgentResources fit one agent reporting on one database server.
func defaultAgentResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
	}
}
