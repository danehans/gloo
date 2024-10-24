package query

import (
	"context"
	"fmt"

	"github.com/solo-io/go-utils/contextutils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	"github.com/solo-io/gloo/projects/gateway2/crds"
	"github.com/solo-io/gloo/projects/gateway2/translator/backendref"
	"github.com/solo-io/gloo/projects/gateway2/wellknown"
)

// RouteInfo contains pre-resolved backends (Services, Upstreams and delegated xRoutes)
// This allows all querying to happen upfront, and detailed logic for delegation to happen
// as part of translation.
type RouteInfo struct {
	// Object is the generic route object which could be HTTPRoute, TCPRoute, etc.
	Object client.Object

	// ParentRef points to the Gateway and optionally Listener or another Route (for HTTPRoute types)
	ParentRef gwv1.ParentReference

	// Hostname overrides to replace the Route hostnames with those that intersect the attached
	// listener's hostname(s) for HTTPRoute types.
	HostnameOverrides []string

	// Backends are pre-resolved here. This list will not contain delegates.
	Backends BackendMap[client.Object]

	// Children contains all delegate routes referenced in any rule of this Route (for HTTPRoute types).
	Children BackendMap[[]*RouteInfo]
}

// GetKind returns the kind of the route.
func (r RouteInfo) GetKind() string {
	return r.Object.GetObjectKind().GroupVersionKind().Kind
}

// GetName returns the name of the route.
func (r RouteInfo) GetName() string {
	return r.Object.GetName()
}

// GetNamespace returns the namespace of the route.
func (r RouteInfo) GetNamespace() string {
	return r.Object.GetNamespace()
}

// Hostnames returns the hostname overrides if they exist, otherwise it returns
// the hostnames specified in the HTTPRoute.
func (r *RouteInfo) Hostnames() []string {
	if len(r.HostnameOverrides) > 0 {
		return r.HostnameOverrides
	}

	httpRoute, ok := r.Object.(*gwv1.HTTPRoute)
	if !ok {
		return []string{}
	}

	strs := make([]string, 0, len(httpRoute.Spec.Hostnames))
	for _, v := range httpRoute.Spec.Hostnames {
		strs = append(strs, string(v))
	}
	return strs
}

// GetBackendForRef fetches a backend for a given BackendObjectReference.
func (r *RouteInfo) GetBackendForRef(backendRef gwv1.BackendObjectReference) (client.Object, error) {
	return r.Backends.get(backendRef, nil)
}

// GetChildrenForRef fetches child routes for a given BackendObjectReference.
func (r *RouteInfo) GetChildrenForRef(backendRef gwv1.BackendObjectReference) ([]*RouteInfo, error) {
	return r.Children.get(backendRef, nil)
}

// Clone creates a deep copy of the RouteInfo object.
func (r *RouteInfo) Clone() *RouteInfo {
	if r == nil {
		return nil
	}
	// TODO (danehans): Why are hostnameOverrides not being clones?
	return &RouteInfo{
		Object:    r.Object,
		ParentRef: r.ParentRef,
		Backends:  r.Backends,
		Children:  r.Children,
	}
}

// UniqueRouteName returns a unique name for the route, based on the index of the route rule and route match.
func (r *RouteInfo) UniqueRouteName(ruleIdx, matchIdx int) string {
	return fmt.Sprintf("%s-%s-%d-%d", r.GetName(), r.GetNamespace(), ruleIdx, matchIdx)
}

// GetRouteChain recursively resolves all backends for the given route object.
// It handles delegation of HTTPRoutes and resolves child routes.
func (r *gatewayQueries) GetRouteChain(
	ctx context.Context,
	route client.Object,
	hostnames []string,
	parentRef gwv1.ParentReference,
) *RouteInfo {
	var backends BackendMap[client.Object]
	var children BackendMap[[]*RouteInfo]

	switch typedRoute := route.(type) {
	case *gwv1.HTTPRoute:
		backends = r.resolveRouteBackends(ctx, typedRoute)
		children = r.getDelegatedChildren(ctx, typedRoute, nil)
	case *gwv1a2.TCPRoute:
		backends = r.resolveRouteBackends(ctx, typedRoute)
		// TODO (danehans): Should route delegation support be added for TCPRoute?
	default:
		contextutils.LoggerFrom(ctx).Errorf("Unsupported route kind: %s", route.GetObjectKind().GroupVersionKind().Kind)
		return nil
	}

	return &RouteInfo{
		Object:            route,
		HostnameOverrides: hostnames,
		ParentRef:         parentRef,
		Backends:          backends,
		Children:          children,
	}
}

func (r *gatewayQueries) allowedRoutes(gw *gwv1.Gateway, l *gwv1.Listener) (func(string) bool, []metav1.GroupKind, error) {
	var allowedKinds []metav1.GroupKind

	// Determine the allowed route kinds based on the listener's protocol
	switch l.Protocol {
	case gwv1.HTTPSProtocolType:
		fallthrough
	case gwv1.HTTPProtocolType:
		allowedKinds = []metav1.GroupKind{{Kind: wellknown.HTTPRouteKind, Group: gwv1.GroupName}}
	case gwv1.TLSProtocolType:
		fallthrough
	case gwv1.TCPProtocolType:
		allowedKinds = []metav1.GroupKind{{Kind: wellknown.TCPRouteKind, Group: gwv1a2.GroupName}}
	case gwv1.UDPProtocolType:
		allowedKinds = []metav1.GroupKind{{}}
	default:
		// allow custom protocols to work
		allowedKinds = []metav1.GroupKind{{Kind: wellknown.HTTPRouteKind, Group: gwv1.GroupName}}
	}

	allowedNs := SameNamespace(gw.Namespace)
	if ar := l.AllowedRoutes; ar != nil {
		// Override the allowed route kinds if specified in AllowedRoutes
		if ar.Kinds != nil {
			allowedKinds = nil // Reset to include only explicitly allowed kinds
			for _, k := range ar.Kinds {
				gk := metav1.GroupKind{Kind: string(k.Kind)}
				if k.Group != nil {
					gk.Group = string(*k.Group)
				} else {
					gk.Group = gwv1.GroupName
				}
				allowedKinds = append(allowedKinds, gk)
			}
		}

		// Determine the allowed namespaces if specified
		if ar.Namespaces != nil && ar.Namespaces.From != nil {
			switch *ar.Namespaces.From {
			case gwv1.NamespacesFromAll:
				allowedNs = AllNamespace()
			case gwv1.NamespacesFromSelector:
				if ar.Namespaces.Selector == nil {
					contextutils.LoggerFrom(context.Background()).Error("Namespace selector must be set")
					return nil, nil, fmt.Errorf("selector must be set")
				}
				selector, err := metav1.LabelSelectorAsSelector(ar.Namespaces.Selector)
				if err != nil {
					contextutils.LoggerFrom(context.Background()).Errorf("Error parsing label selector: %v", err)
					return nil, nil, err
				}
				allowedNs = r.NamespaceSelector(selector)
			}
		}
	}

	contextutils.LoggerFrom(context.Background()).Debugf("Allowed route kinds: %v", allowedKinds)
	return allowedNs, allowedKinds, nil
}

func (r *gatewayQueries) resolveRouteBackends(ctx context.Context, obj client.Object) BackendMap[client.Object] {
	out := NewBackendMap[client.Object]()

	processBackendRefs := func(refs []gwv1.BackendObjectReference) {
		for _, backendRef := range refs {
			refObj, err := r.GetBackendForRef(ctx, r.ObjToFrom(obj), &backendRef)
			if err != nil {
				out.AddError(backendRef, err)
				continue
			}
			out.Add(backendRef, refObj)
		}
	}

	switch rt := obj.(type) {
	case *gwv1.HTTPRoute:
		for _, rule := range rt.Spec.Rules {
			var refs []gwv1.BackendObjectReference
			for _, ref := range rule.BackendRefs {
				refs = append(refs, ref.BackendObjectReference)
			}
			processBackendRefs(refs)
		}
	case *gwv1a2.TCPRoute:
		for _, rule := range rt.Spec.Rules {
			var refs []gwv1.BackendObjectReference
			for _, ref := range rule.BackendRefs {
				refs = append(refs, ref.BackendObjectReference)
			}
			processBackendRefs(refs)
		}
	default:
		contextutils.LoggerFrom(ctx).Errorf("unsupported route type: %T", obj)
		return out
	}

	return out
}

func (r *gatewayQueries) getDelegatedChildren(
	ctx context.Context,
	parent *gwv1.HTTPRoute,
	visited sets.Set[types.NamespacedName],
) BackendMap[[]*RouteInfo] {
	// Initialize the set of visited routes if it hasn't been initialized yet
	if visited == nil {
		visited = sets.New[types.NamespacedName]()
	}
	parentRef := namespacedName(parent)
	visited.Insert(parentRef)

	children := NewBackendMap[[]*RouteInfo]()
	for _, parentRule := range parent.Spec.Rules {
		var refChildren []*RouteInfo
		for _, backendRef := range parentRule.BackendRefs {
			// Check if the backend reference is an HTTPRoute
			if !backendref.RefIsHTTPRoute(backendRef.BackendObjectReference) {
				continue
			}
			// Fetch child routes based on the backend reference
			referencedRoutes, err := r.fetchChildRoutes(ctx, parent.Namespace, backendRef)
			if err != nil {
				// Log the error using contextutils and add it to the children map
				contextutils.LoggerFrom(ctx).Errorf("Failed to fetch child routes: %v", err)
				children.AddError(backendRef.BackendObjectReference, err)
				continue
			}
			for _, childRoute := range referencedRoutes {
				childRef := namespacedName(&childRoute)
				if visited.Has(childRef) {
					err := fmt.Errorf("ignoring child route %s for parent %s: %w", parentRef, childRef, ErrCyclicReference)
					contextutils.LoggerFrom(ctx).Warnf("Detected cyclic reference for child route %s and parent %s", childRef, parentRef)
					children.AddError(backendRef.BackendObjectReference, err)
					// Don't resolve invalid child route
					continue
				}
				// Recursively get the route chain for each child route
				routeInfo := &RouteInfo{
					Object: &childRoute,
					ParentRef: gwv1.ParentReference{
						Group:     ptr.To(gwv1.Group(wellknown.GatewayGroup)),
						Kind:      ptr.To(gwv1.Kind(wellknown.HTTPRouteKind)),
						Namespace: ptr.To(gwv1.Namespace(parent.Namespace)),
						Name:      gwv1.ObjectName(parent.Name),
					},
					Backends: r.resolveRouteBackends(ctx, &childRoute),
					Children: r.getDelegatedChildren(ctx, &childRoute, visited),
				}
				refChildren = append(refChildren, routeInfo)
			}
			// Add the resolved children routes to the backend map
			children.Add(backendRef.BackendObjectReference, refChildren)
		}
	}
	return children
}

func (r *gatewayQueries) fetchChildRoutes(
	ctx context.Context,
	parentNamespace string,
	backendRef gwv1.HTTPBackendRef,
) ([]gwv1.HTTPRoute, error) {
	delegatedNs := parentNamespace
	if !backendref.RefIsHTTPRoute(backendRef.BackendObjectReference) {
		return nil, nil
	}

	// Use the namespace specified in the backend reference if available
	if backendRef.Namespace != nil {
		delegatedNs = string(*backendRef.Namespace)
	}

	var refChildren []gwv1.HTTPRoute
	if string(backendRef.Name) == "" || string(backendRef.Name) == "*" {
		// Handle wildcard references by listing all HTTPRoutes in the specified namespace
		var rlist gwv1.HTTPRouteList
		err := r.client.List(ctx, &rlist, client.InNamespace(delegatedNs))
		if err != nil {
			contextutils.LoggerFrom(ctx).Errorf("Failed to list HTTPRoutes in namespace %s: %v", delegatedNs, err)
			return nil, err
		}
		refChildren = append(refChildren, rlist.Items...)
	} else {
		// Lookup a specific child route by its name
		delegatedRef := types.NamespacedName{
			Namespace: delegatedNs,
			Name:      string(backendRef.Name),
		}

		child := &gwv1.HTTPRoute{}
		err := r.client.Get(ctx, delegatedRef, child)
		if err != nil {
			contextutils.LoggerFrom(ctx).Errorf("Failed to get HTTPRoute %s: %v", delegatedRef.String(), err)
			return nil, err
		}
		refChildren = append(refChildren, *child)
	}

	// Check if no child routes were resolved and log an error if needed
	if len(refChildren) == 0 {
		contextutils.LoggerFrom(ctx).Errorf("No child routes resolved for backendRef %s", backendRef.Name)
		return nil, ErrUnresolvedReference
	}

	return refChildren, nil
}

func (r *gatewayQueries) GetRoutesForGateway(ctx context.Context, gw *gwv1.Gateway) (*RoutesForGwResult, error) {
	ctx = contextutils.WithLogger(ctx, "GetRoutesForGateway")
	ret := &RoutesForGwResult{
		ListenerResults: map[string]*ListenerResult{},
	}

	nns := types.NamespacedName{
		Namespace: gw.Namespace,
		Name:      gw.Name,
	}

	// TODO (danehans): Use the discovery client to check if the tcproutes resource is available.
	routeListTypes := []client.ObjectList{
		&gwv1.HTTPRouteList{},
		&gwv1a2.TCPRouteList{},
	}

	var routes []client.Object
	for _, routeList := range routeListTypes {
		if err := fetchRoutes(ctx, r, routeList, nns, &routes); err != nil {
			return ret, err
		}
	}

	// Process each route
	for _, route := range routes {
		if err := r.processRoute(ctx, gw, route, ret); err != nil {
			logError(ctx, err, "Error processing route")
		}
	}

	return ret, nil
}

// fetchRoutes is a helper function to fetch routes and add to the routes slice.
func fetchRoutes(ctx context.Context, r *gatewayQueries, routeList client.ObjectList, nns types.NamespacedName, routes *[]client.Object) error {
	listAndAppendRoutes := func(list client.ObjectList, fieldSelector string) error {
		if err := r.client.List(ctx, list, client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector(fieldSelector, nns.String())}); err != nil {
			return fmt.Errorf("failed to list routes: %w", err)
		}
		routeItems, err := crds.GetRouteItems(list)
		if err != nil {
			return fmt.Errorf("failed to get route items: %w", err)
		}
		*routes = append(*routes, routeItems...)
		return nil
	}

	switch list := routeList.(type) {
	case *gwv1.HTTPRouteList:
		if err := listAndAppendRoutes(list, HttpRouteTargetField); err != nil {
			return fmt.Errorf("failed to list HTTPRoutes: %w", err)
		}
	case *gwv1a2.TCPRouteList:
		if err := listAndAppendRoutes(list, TcpRouteTargetField); err != nil {
			return fmt.Errorf("failed to list TCPRoutes: %w", err)
		}
	default:
		return fmt.Errorf("unsupported route list type: %T", list)
	}

	return nil
}

func (r *gatewayQueries) processRoute(ctx context.Context, gw *gwv1.Gateway, route client.Object, ret *RoutesForGwResult) error {
	refs := getParentRefsForGw(gw, route)
	routeKind := route.GetObjectKind().GroupVersionKind().Kind

	for _, ref := range refs {
		anyRoutesAllowed := false
		anyListenerMatched := false
		anyHostsMatch := false

		for _, l := range gw.Spec.Listeners {
			lr := ret.ListenerResults[string(l.Name)]
			if lr == nil {
				lr = &ListenerResult{}
				ret.ListenerResults[string(l.Name)] = lr
			}

			allowedNs, allowedKinds, err := r.allowedRoutes(gw, &l)
			if err != nil {
				lr.Error = err
				continue
			}

			// Check if the kind of the route is allowed by the listener
			if !isKindAllowed(routeKind, allowedKinds) {
				continue
			}

			// Check if the namespace of the route is allowed by the listener
			if !allowedNs(route.GetNamespace()) {
				continue
			}
			anyRoutesAllowed = true

			// Check if the listener matches the route's parent reference
			if !parentRefMatchListener(&ref, &l) {
				continue
			}
			anyListenerMatched = true

			// If the route is an HTTPRoute, check the hostname intersection
			var hostnames []string
			if routeKind == wellknown.HTTPRouteKind {
				if hr, ok := route.(*gwv1.HTTPRoute); ok {
					var ok bool
					ok, hostnames = hostnameIntersect(&l, hr)
					if !ok {
						continue
					}
					anyHostsMatch = true
				}
			}

			// If all checks pass, add the route to the listener result
			lr.Routes = append(lr.Routes, r.GetRouteChain(ctx, route, hostnames, ref))
		}

		// Handle route errors based on checks
		if !anyRoutesAllowed {
			ret.RouteErrors = append(ret.RouteErrors, &RouteError{
				Route:     route,
				ParentRef: ref,
				Error:     Error{E: ErrNotAllowedByListeners, Reason: gwv1.RouteReasonNotAllowedByListeners},
			})
		} else if !anyListenerMatched {
			ret.RouteErrors = append(ret.RouteErrors, &RouteError{
				Route:     route,
				ParentRef: ref,
				Error:     Error{E: ErrNoMatchingParent, Reason: gwv1.RouteReasonNoMatchingParent},
			})
		} else if routeKind == wellknown.HTTPRouteKind && !anyHostsMatch {
			ret.RouteErrors = append(ret.RouteErrors, &RouteError{
				Route:     route,
				ParentRef: ref,
				Error:     Error{E: ErrNoMatchingListenerHostname, Reason: gwv1.RouteReasonNoMatchingListenerHostname},
			})
		}
	}

	return nil
}

// logError is used for structured error handling to avoid ignoring errors.
func logError(ctx context.Context, err error, message string) {
	if err != nil {
		contextutils.LoggerFrom(ctx).Errorf("%s: %v", message, err)
	}
}

// isKindAllowed is a helper function to check if a kind is allowed.
func isKindAllowed(routeKind string, allowedKinds []metav1.GroupKind) bool {
	for _, kind := range allowedKinds {
		if kind.Kind == routeKind {
			return true
		}
	}
	return false
}

type Namespaced interface {
	GetName() string
	GetNamespace() string
}

func namespacedName(o Namespaced) types.NamespacedName {
	return types.NamespacedName{Name: o.GetName(), Namespace: o.GetNamespace()}
}
