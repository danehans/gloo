package wellknown

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

var (
	SecretGVK         = corev1.SchemeGroupVersion.WithKind("Secret")
	ConfigMapGVK      = corev1.SchemeGroupVersion.WithKind("ConfigMap")
	ServiceGVK        = corev1.SchemeGroupVersion.WithKind("Service")
	ServiceAccountGVK = corev1.SchemeGroupVersion.WithKind("ServiceAccount")
	CrdGVK            = apiextv1.SchemeGroupVersion.WithKind("CustomResourceDefinition")
	DeploymentGVK     = appsv1.SchemeGroupVersion.WithKind("Deployment")
)
