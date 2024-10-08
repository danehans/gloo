package crds

import (
	"context"
	"fmt"
	"log"
	_ "embed"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

//go:embed gateway-crds.yaml
var GatewayCrds []byte


// IsResourceSupported checks if a particular resource exists in the Kubernetes cluster
func IsResourceSupported(ctx context.Context, restConfig *rest.Config, resourceName, groupVersion string) bool {
	// Create a new Kubernetes client using the provided rest.Config
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Printf("Error creating Kubernetes client: %v", err)
		return false
	}

	// Use the Discovery API to check if the resource is available in the cluster
	apiResourceList, err := kubeClient.Discovery().ServerResourcesForGroupVersion(groupVersion)
	if err != nil {
		// Log an error if the group/version does not exist and return false
		log.Printf("Resource group/version %s is not supported: %v", groupVersion, err)
		return false
	}

	for _, resource := range apiResourceList.APIResources {
		if resource.Name == resourceName {
			return true
		}
	}
	return false
}

// GetRouteItems is a helper function to extract the list of route items from a client.ObjectList.
// Supported route list types are:
//
//  * HTTPRouteList
//  * TCPRouteList
//
func GetRouteItems(list client.ObjectList) ([]client.Object, error) {
	switch routes := list.(type) {
	case *gwv1.HTTPRouteList:
		var objs []client.Object
		for i := range routes.Items {
			objs = append(objs, &routes.Items[i])
		}
		return objs, nil
	case *gwv1a2.TCPRouteList:
		var objs []client.Object
		for i := range routes.Items {
			objs = append(objs, &routes.Items[i])
		}
		return objs, nil
	default:
		return nil, fmt.Errorf("unsupported route type %T", list)
	}
}
