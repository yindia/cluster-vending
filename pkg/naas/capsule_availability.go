package naas

import (
	"context"
	"fmt"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ObserveCapsuleAvailability is a fresh, read-only fallback when recorded Helm
// progress is not Ready. The pinned bootstrap fixes the controller/service names
// and service account. An old ReplicaSet may still serve during a rolling update;
// neither the new revision nor all desired replicas need to have converged.
// This is availability evidence, not runtime isolation qualification. Callers
// must separately verify the Capsule API and exact shared bootstrap payload.
func ObserveCapsuleAvailability(ctx context.Context, reader client.Reader) error {
	const ns = "capsule-system"
	deployment := &appsv1.Deployment{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: "capsule-controller-manager"}, deployment); err != nil {
		return fmt.Errorf("read Capsule controller Deployment: %w", err)
	}
	if deployment.UID == "" || !deployment.DeletionTimestamp.IsZero() || deployment.Status.AvailableReplicas < 1 || deployment.Spec.Template.Spec.ServiceAccountName != "capsule" || deployment.Spec.Selector == nil {
		return fmt.Errorf("waiting for an available trusted Capsule controller replica")
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil || selector.Empty() {
		return fmt.Errorf("capsule controller selector is unavailable")
	}
	service := &corev1.Service{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: "capsule-webhook-service"}, service); err != nil {
		return fmt.Errorf("read Capsule webhook Service: %w", err)
	}
	if service.UID == "" || !service.DeletionTimestamp.IsZero() || len(service.Spec.Selector) == 0 {
		return fmt.Errorf("capsule webhook Service has no controller selector")
	}
	webhookConfigs := &admissionv1.ValidatingWebhookConfigurationList{}
	if err := reader.List(ctx, webhookConfigs); err != nil {
		return fmt.Errorf("read Capsule validating webhooks: %w", err)
	}
	webhookPort := int32(0)
	for _, config := range webhookConfigs.Items {
		if config.Name != "capsule-validating-webhook-configuration" && config.Name != "capsule-dynamic-webhook" {
			continue
		}
		if !config.DeletionTimestamp.IsZero() {
			continue
		}
		for _, hook := range config.Webhooks {
			target := hook.ClientConfig.Service
			if hook.Name != "tenants.validating.projectcapsule.dev" || target == nil || target.Namespace != ns || target.Name != service.Name || len(hook.ClientConfig.CABundle) == 0 {
				continue
			}
			webhookPort = 443
			if target.Port != nil {
				webhookPort = *target.Port
			}
		}
	}
	portName := ""
	targetPort := int32(0)
	foundPort := false
	for _, port := range service.Spec.Ports {
		if port.Port == webhookPort && (port.Protocol == "" || port.Protocol == corev1.ProtocolTCP) {
			foundPort = true
			portName = port.Name
			targetPort = port.TargetPort.IntVal
			if targetPort == 0 && port.TargetPort.StrVal == "" {
				targetPort = port.Port
			}
			break
		}
	}
	if !foundPort {
		return fmt.Errorf("waiting for the Capsule Tenant validating webhook and matching Service port")
	}
	endpoints := &discoveryv1.EndpointSliceList{}
	if err := reader.List(ctx, endpoints, client.InNamespace(ns), client.MatchingLabels{discoveryv1.LabelServiceName: service.Name}); err != nil {
		return fmt.Errorf("read Capsule webhook endpoints: %w", err)
	}
	for _, slice := range endpoints.Items {
		if !slice.DeletionTimestamp.IsZero() || !metav1.IsControlledBy(&slice, service) {
			continue
		}
		hasPort := false
		for _, port := range slice.Ports {
			if port.Port != nil && *port.Port > 0 && (targetPort == 0 || *port.Port == targetPort) && port.Name != nil && *port.Name == portName && (port.Protocol == nil || *port.Protocol == corev1.ProtocolTCP) {
				hasPort = true
			}
		}
		if !hasPort {
			continue
		}
		for _, endpoint := range slice.Endpoints {
			ref := endpoint.TargetRef
			if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready || endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating || len(endpoint.Addresses) == 0 || ref == nil || ref.Kind != "Pod" || ref.Namespace != ns || ref.UID == "" {
				continue
			}
			pod := &corev1.Pod{}
			if err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, pod); err != nil {
				return fmt.Errorf("read Capsule serving Pod: %w", err)
			}
			if pod.UID != ref.UID || !pod.DeletionTimestamp.IsZero() || pod.Spec.ServiceAccountName != "capsule" || !selector.Matches(labels.Set(pod.Labels)) || !labels.SelectorFromSet(service.Spec.Selector).Matches(labels.Set(pod.Labels)) {
				continue
			}
			ready := false
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					ready = true
				}
			}
			owner := metav1.GetControllerOf(pod)
			if !ready || owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "ReplicaSet" {
				continue
			}
			replicas := &appsv1.ReplicaSet{}
			if err := reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: owner.Name}, replicas); err != nil {
				return fmt.Errorf("read Capsule serving ReplicaSet: %w", err)
			}
			if replicas.UID == owner.UID && replicas.DeletionTimestamp.IsZero() && metav1.IsControlledBy(replicas, deployment) {
				return nil
			}
		}
	}
	return fmt.Errorf("waiting for a ready Capsule webhook endpoint backed by a healthy owned controller Pod")
}
