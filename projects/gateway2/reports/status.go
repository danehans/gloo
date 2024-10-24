package reports

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/solo-io/go-utils/contextutils"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

// TODO: refactor this struct + methods to better reflect the usage now in proxy_syncer
var (
	missingGatewayReportErr = "building status for Gateway '%s' (namespace: '%s') but no GatewayReport was present"
	missingRouteReportErr   = "building status for %s '%s' (namespace: '%s') but no RouteReport was present"
)

func (r *ReportMap) BuildGWStatus(ctx context.Context, gw gwv1.Gateway) *gwv1.GatewayStatus {
	gwReport := r.Gateway(&gw)
	if gwReport == nil {
		return nil
	}

	finalListeners := make([]gwv1.ListenerStatus, 0, len(gw.Spec.Listeners))
	for _, lis := range gw.Spec.Listeners {
		lisReport := gwReport.listener(&lis)
		addMissingListenerConditions(lisReport)

		finalConditions := make([]metav1.Condition, 0, len(lisReport.Status.Conditions))
		oldLisStatusIndex := slices.IndexFunc(gw.Status.Listeners, func(l gwv1.ListenerStatus) bool {
			return l.Name == lis.Name
		})
		for _, lisCondition := range lisReport.Status.Conditions {
			lisCondition.ObservedGeneration = gwReport.observedGeneration

			// copy old condition from gw so LastTransitionTime is set correctly below by SetStatusCondition()
			if oldLisStatusIndex != -1 {
				if cond := meta.FindStatusCondition(gw.Status.Listeners[oldLisStatusIndex].Conditions, lisCondition.Type); cond != nil {
					finalConditions = append(finalConditions, *cond)
				}
			}
			meta.SetStatusCondition(&finalConditions, lisCondition)
		}
		lisReport.Status.Conditions = finalConditions
		finalListeners = append(finalListeners, lisReport.Status)
	}

	addMissingGatewayConditions(r.Gateway(&gw))

	finalConditions := make([]metav1.Condition, 0)
	for _, gwCondition := range gwReport.GetConditions() {
		gwCondition.ObservedGeneration = gwReport.observedGeneration

		// copy old condition from gw so LastTransitionTime is set correctly below by SetStatusCondition()
		if cond := meta.FindStatusCondition(gw.Status.Conditions, gwCondition.Type); cond != nil {
			finalConditions = append(finalConditions, *cond)
		}
		meta.SetStatusCondition(&finalConditions, gwCondition)
	}

	finalGwStatus := gwv1.GatewayStatus{}
	finalGwStatus.Conditions = finalConditions
	finalGwStatus.Listeners = finalListeners
	return &finalGwStatus
}

// BuildRouteStatus returns a newly constructed and fully defined RouteStatus for the supplied route object
// according to the state of the ReportMap. If the ReportMap does not have a RouteReport for the given route,
// e.g. because it did not encounter the route during translation, or the object is an unsupported route type,
// nil is returned. The following object types are supported:
//
// * HTTPRoute
// * TCPRoute
func (r *ReportMap) BuildRouteStatus(ctx context.Context, obj client.Object, cName string) *gwv1.RouteStatus {
	var routeStatus gwv1.RouteStatus
	var parentRefs []gwv1.ParentReference
	var routeReport *RouteReport

	contextutils.LoggerFrom(ctx).Debugf("building status for %s %s/%s",
		obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(),
		obj.GetName())

	// Handle the type assertion to check if the input object is a supported Gateway API route type
	switch route := obj.(type) {
	case *gwv1.HTTPRoute:
		routeReport = r.route(route)
		parentRefs = append(parentRefs, route.Spec.ParentRefs...)
		if len(parentRefs) == 0 {
			parentRefs = append(parentRefs, routeReport.parentRefs()...)
		}
	case *gwv1a2.TCPRoute:
		routeReport = r.route(route)
		parentRefs = append(parentRefs, route.Spec.ParentRefs...)
		if len(parentRefs) == 0 {
			parentRefs = append(parentRefs, routeReport.parentRefs()...)
		}
	default:
		// Unsupported route type
		contextutils.LoggerFrom(ctx).Error(fmt.Errorf("unsupported route type %T", obj), "failed to build route status")
		return nil
	}

	if routeReport == nil {
		// Log a message and nreturn early if the route report is missing
		contextutils.LoggerFrom(ctx).Infof("missing route report for %T %s/%s", obj, obj.GetName(), obj.GetNamespace())
		return nil
	}

	contextutils.LoggerFrom(ctx).Infof("building status for %T %s/%s", obj, obj.GetNamespace(), obj.GetName())

	// Process the parent references to build the RouteParentStatus
	for _, parentRef := range parentRefs {
		parentStatusReport := routeReport.parentRef(&parentRef)
		addMissingParentRefConditions(parentStatusReport)

		// Get the status of the current parentRef conditions if they exist
		var currentParentRefConditions []metav1.Condition
		currentParentRefIdx := slices.IndexFunc(routeStatus.Parents, func(s gwv1.RouteParentStatus) bool {
			return reflect.DeepEqual(s.ParentRef, parentRef)
		})
		if currentParentRefIdx != -1 {
			currentParentRefConditions = routeStatus.Parents[currentParentRefIdx].Conditions
		}

		finalConditions := make([]metav1.Condition, 0, len(parentStatusReport.Conditions))
		for _, pCondition := range parentStatusReport.Conditions {
			pCondition.ObservedGeneration = routeReport.observedGeneration

			// Copy old condition to preserve LastTransitionTime, if it exists
			if cond := meta.FindStatusCondition(currentParentRefConditions, pCondition.Type); cond != nil {
				finalConditions = append(finalConditions, *cond)
			}
			meta.SetStatusCondition(&finalConditions, pCondition)
		}

		routeParentStatus := gwv1.RouteParentStatus{
			ParentRef:      parentRef,
			ControllerName: gwv1.GatewayController(cName),
			Conditions:     finalConditions,
		}
		routeStatus.Parents = append(routeStatus.Parents, routeParentStatus)
	}

	return &routeStatus
}

// Reports will initially only contain negative conditions found during translation,
// so all missing conditions are assumed to be positive. Here we will add all missing conditions
// to a given report, i.e. set healthy conditions
func addMissingGatewayConditions(gwReport *GatewayReport) {
	if cond := meta.FindStatusCondition(gwReport.GetConditions(), string(gwv1.GatewayConditionAccepted)); cond == nil {
		gwReport.SetCondition(GatewayCondition{
			Type:   gwv1.GatewayConditionAccepted,
			Status: metav1.ConditionTrue,
			Reason: gwv1.GatewayReasonAccepted,
		})
	}
	if cond := meta.FindStatusCondition(gwReport.GetConditions(), string(gwv1.GatewayConditionProgrammed)); cond == nil {
		gwReport.SetCondition(GatewayCondition{
			Type:   gwv1.GatewayConditionProgrammed,
			Status: metav1.ConditionTrue,
			Reason: gwv1.GatewayReasonProgrammed,
		})
	}
}

// Reports will initially only contain negative conditions found during translation,
// so all missing conditions are assumed to be positive. Here we will add all missing conditions
// to a given report, i.e. set healthy conditions
func addMissingListenerConditions(lisReport *ListenerReport) {
	// set healthy conditions for Condition Types not set yet (i.e. no negative status yet, we can assume positive)
	if cond := meta.FindStatusCondition(lisReport.Status.Conditions, string(gwv1.ListenerConditionAccepted)); cond == nil {
		lisReport.SetCondition(ListenerCondition{
			Type:   gwv1.ListenerConditionAccepted,
			Status: metav1.ConditionTrue,
			Reason: gwv1.ListenerReasonAccepted,
		})
	}
	if cond := meta.FindStatusCondition(lisReport.Status.Conditions, string(gwv1.ListenerConditionConflicted)); cond == nil {
		lisReport.SetCondition(ListenerCondition{
			Type:   gwv1.ListenerConditionConflicted,
			Status: metav1.ConditionFalse,
			Reason: gwv1.ListenerReasonNoConflicts,
		})
	}
	if cond := meta.FindStatusCondition(lisReport.Status.Conditions, string(gwv1.ListenerConditionResolvedRefs)); cond == nil {
		lisReport.SetCondition(ListenerCondition{
			Type:   gwv1.ListenerConditionResolvedRefs,
			Status: metav1.ConditionTrue,
			Reason: gwv1.ListenerReasonResolvedRefs,
		})
	}
	if cond := meta.FindStatusCondition(lisReport.Status.Conditions, string(gwv1.ListenerConditionProgrammed)); cond == nil {
		lisReport.SetCondition(ListenerCondition{
			Type:   gwv1.ListenerConditionProgrammed,
			Status: metav1.ConditionTrue,
			Reason: gwv1.ListenerReasonProgrammed,
		})
	}
}

// Reports will initially only contain negative conditions found during translation,
// so all missing conditions are assumed to be positive. Here we will add all missing conditions
// to a given report, i.e. set healthy conditions
func addMissingParentRefConditions(report *ParentRefReport) {
	if cond := meta.FindStatusCondition(report.Conditions, string(gwv1.RouteConditionAccepted)); cond == nil {
		report.SetCondition(RouteCondition{
			Type:   gwv1.RouteConditionAccepted,
			Status: metav1.ConditionTrue,
			Reason: gwv1.RouteReasonAccepted,
		})
	}
	if cond := meta.FindStatusCondition(report.Conditions, string(gwv1.RouteConditionResolvedRefs)); cond == nil {
		report.SetCondition(RouteCondition{
			Type:   gwv1.RouteConditionResolvedRefs,
			Status: metav1.ConditionTrue,
			Reason: gwv1.RouteReasonResolvedRefs,
		})
	}
}
