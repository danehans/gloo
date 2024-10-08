package proxy_syncer

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/solo-io/gloo/pkg/utils/statsutils"
	"github.com/solo-io/gloo/projects/gateway2/extensions"
	"github.com/solo-io/gloo/projects/gateway2/reports"
	gwplugins "github.com/solo-io/gloo/projects/gateway2/translator/plugins"
	"github.com/solo-io/gloo/projects/gateway2/translator/plugins/registry"
	gloo_solo_io "github.com/solo-io/gloo/projects/gloo/pkg/api/v1"
	"github.com/solo-io/gloo/projects/gloo/pkg/utils"
	"github.com/solo-io/go-utils/contextutils"
	"github.com/solo-io/solo-kit/pkg/api/v1/clients"
	"github.com/solo-io/solo-kit/pkg/utils/statusutils"
	//"k8s.io/client-go/kubernetes"
	//"k8s.io/client-go/rest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

// QueueStatusForProxiesFn queues a list of proxies to be synced and the plugin registry that produced them for a given sync iteration
type QueueStatusForProxiesFn func(ctx context.Context, proxies gloo_solo_io.ProxyList, pluginRegistry *registry.PluginRegistry, totalSyncCount int)

// ProxySyncer is responsible for translating Kubernetes Gateway CRs into Gloo Proxies
// and syncing the proxyClient with the newly translated proxies.
type ProxySyncer struct {
	controllerName string
	writeNamespace string

	inputs          *GatewayInputChannels
	mgr             manager.Manager
	k8sGwExtensions extensions.K8sGatewayExtensions

	// proxyReconciler wraps the client that writes Proxy resources into an in-memory cache
	// This cache is utilized by the debug.ProxyEndpointServer
	proxyReconciler gloo_solo_io.ProxyReconciler

	// queueStatusForProxies stores a list of proxies that need the proxy status synced and the plugin registry
	// that produced them for a given sync iteration
	queueStatusForProxies QueueStatusForProxiesFn
}

type GatewayInputChannels struct {
	genericEvent AsyncQueue[struct{}]
	secretEvent  AsyncQueue[SecretInputs]
}

func (x *GatewayInputChannels) Kick(ctx context.Context) {
	x.genericEvent.Enqueue(struct{}{})
}

func (x *GatewayInputChannels) UpdateSecretInputs(ctx context.Context, inputs SecretInputs) {
	x.secretEvent.Enqueue(inputs)
}

func NewGatewayInputChannels() *GatewayInputChannels {
	return &GatewayInputChannels{
		genericEvent: NewAsyncQueue[struct{}](),
		secretEvent:  NewAsyncQueue[SecretInputs](),
	}
}

// labels used to uniquely identify Proxies that are managed by the kube gateway controller
var kubeGatewayProxyLabels = map[string]string{
	// the proxy type key/value must stay in sync with the one defined in projects/gateway2/translator/gateway_translator.go
	utils.ProxyTypeKey: utils.GatewayApiProxyValue,
}

// NewProxySyncer returns an implementation of the ProxySyncer
// The provided GatewayInputChannels are used to trigger syncs.
// The proxy sync is triggered by the `genericEvent` which is kicked when
// we reconcile gateway in the gateway controller. The `secretEvent` is kicked when a secret is created, updated,
func NewProxySyncer(
	controllerName, writeNamespace string,
	inputs *GatewayInputChannels,
	mgr manager.Manager,
	k8sGwExtensions extensions.K8sGatewayExtensions,
	proxyClient gloo_solo_io.ProxyClient,
	queueStatusForProxies QueueStatusForProxiesFn,
) *ProxySyncer {
	return &ProxySyncer{
		controllerName:        controllerName,
		writeNamespace:        writeNamespace,
		inputs:                inputs,
		mgr:                   mgr,
		k8sGwExtensions:       k8sGwExtensions,
		proxyReconciler:       gloo_solo_io.NewProxyReconciler(proxyClient, statusutils.NewNoOpStatusClient()),
		queueStatusForProxies: queueStatusForProxies,
	}
}

func (s *ProxySyncer) Start(ctx context.Context) error {
	ctx = contextutils.WithLogger(ctx, "k8s-gw-syncer")
	contextutils.LoggerFrom(ctx).Debug("starting syncer for k8s gateway proxies")

	var (
		secretsWarmed bool
		// totalResyncs is used to track the number of times the proxy syncer has been triggered
		totalResyncs int
	)
	resyncProxies := func() {
		if !secretsWarmed {
			return
		}
		totalResyncs++
		contextutils.LoggerFrom(ctx).Debugf("resyncing k8s gateway proxies [%v]", totalResyncs)
		stopwatch := statsutils.NewTranslatorStopWatch("ProxySyncer")
		stopwatch.Start()
		var (
			proxies gloo_solo_io.ProxyList
		)
		defer func() {
			duration := stopwatch.Stop(ctx)
			contextutils.LoggerFrom(ctx).Debugf("translated and wrote %d proxies in %s", len(proxies), duration.String())
		}()

		var gwl gwv1.GatewayList
		err := s.mgr.GetClient().List(ctx, &gwl)
		if err != nil {
			// This should never happen, try again?
			return
		}

		pluginRegistry := s.k8sGwExtensions.CreatePluginRegistry(ctx)
		rm := reports.NewReportMap()
		r := reports.NewReporter(&rm)

		var (
			translatedGateways []gwplugins.TranslatedGateway
		)
		for _, gw := range gwl.Items {
			gatewayTranslator := s.k8sGwExtensions.GetTranslator(ctx, &gw, pluginRegistry)
			if gatewayTranslator == nil {
				contextutils.LoggerFrom(ctx).Errorf("no translator found for Gateway %s (gatewayClass %s)", gw.Name, gw.Spec.GatewayClassName)
				continue
			}
			proxy := gatewayTranslator.TranslateProxy(ctx, &gw, s.writeNamespace, r)
			if proxy != nil {
				// Add proxy id to the proxy metadata to track proxies for status reporting
				proxyAnnotations := proxy.GetMetadata().GetAnnotations()
				if proxyAnnotations == nil {
					proxyAnnotations = make(map[string]string)
				}
				proxyAnnotations[utils.ProxySyncId] = strconv.Itoa(totalResyncs)
				proxy.GetMetadata().Annotations = proxyAnnotations

				proxies = append(proxies, proxy)
				translatedGateways = append(translatedGateways, gwplugins.TranslatedGateway{
					Gateway: gw,
				})
			}
		}

		applyPostTranslationPlugins(ctx, pluginRegistry, &gwplugins.PostTranslationContext{
			TranslatedGateways: translatedGateways,
		})

		s.queueStatusForProxies(ctx, proxies, &pluginRegistry, totalResyncs)
		s.syncStatus(ctx, rm, gwl)
		s.reconcileProxies(ctx, proxies)
	}

	for {
		select {
		case <-ctx.Done():
			contextutils.LoggerFrom(ctx).Debug("context done, stopping proxy syncer")
			return nil
		case <-s.inputs.genericEvent.Next():
			resyncProxies()
		case <-s.inputs.secretEvent.Next():
			secretsWarmed = true
			resyncProxies()
		}
	}
}

func (s *ProxySyncer) syncRouteStatus(ctx context.Context, rm reports.ReportMap) {
	ctx = contextutils.WithLogger(ctx, "routeStatusSyncer")
	logger := contextutils.LoggerFrom(ctx)
	logger.Debugf("syncing k8s gateway route status")
	stopwatch := statsutils.NewTranslatorStopWatch("RouteStatusSyncer")
	stopwatch.Start()
	defer stopwatch.Stop(ctx)

	restConfig := s.mgr.GetConfig()
	// Sometimes the List returns stale (cached) routes, causing the status update to fail
	// with "the object has been modified" errors. Therefore, we try the status updates in a retry loop.
	err := retry.Do(func() error {
		// Define the list of supported route types
		routeListTypes := []client.ObjectList{
			&gwv1.HTTPRouteList{},
		}

		// Check if TCPRoute is supported and only add it to the list if it exists
		if isResourceSupported(ctx, restConfig, "tcproutes", "gateway.networking.k8s.io/v1alpha2") {
			routeListTypes = append(routeListTypes, &gwv1a2.TCPRouteList{})
		}

		for _, list := range routeListTypes {
			err := s.mgr.GetClient().List(ctx, list)
			if err != nil {
				// log this at error level because this is not an expected error
				logger.Error(err)
				return err
			}

			items, err := getRouteItems(list)
			if err != nil {
				logger.Error(err)
				return err
			}

			for _, obj := range items {
				// Build the route status from the report map
				if status := rm.BuildRouteStatus(ctx, obj, s.controllerName); status != nil {
					// Extract the current RouteStatus from the object based on its type
					var currentStatus *gwv1.RouteStatus
					switch route := obj.(type) {
					case *gwv1.HTTPRoute:
						currentStatus = &route.Status.RouteStatus
					case *gwv1a2.TCPRoute:
						currentStatus = &route.Status.RouteStatus
					default:
						logger.Warnf("unsupported route type: %T", obj)
						continue
					}

					// Compare the current status with the generated status and update if they differ
					if !isRouteStatusEqual(currentStatus, status) {
						if err := updateRouteStatus(ctx, obj, status, s.mgr.GetClient()); err != nil {
							logger.Debugw("status update attempt failed",
							"kind",	obj.GetObjectKind(),
							"error", err,
							"namespace", obj.GetNamespace(),
							"name", obj.GetName(),
							)
							return err
						}
					}
				}
			}
		}

		return nil
	},
		retry.Attempts(5),
		retry.Delay(100*time.Millisecond),
		retry.DelayType(retry.BackOffDelay),
	)
	if err != nil {
		logger.Errorw("all attempts failed at updating route statuses", "error", err)
	}
}

// Helper function to update the status of a route
func updateRouteStatus(ctx context.Context, obj client.Object, status *gwv1.RouteStatus, client client.Client) error {
	switch route := obj.(type) {
	case *gwv1.HTTPRoute:
		route.Status = gwv1.HTTPRouteStatus{RouteStatus: *status}
		return client.Status().Update(ctx, route)
	case *gwv1a2.TCPRoute:
		route.Status = gwv1a2.TCPRouteStatus{RouteStatus: *status}
		return client.Status().Update(ctx, route)
	default:
		return fmt.Errorf("unsupported route type %T", obj)
	}
}

// isRouteStatusEqual checks if the two route statuses are equal
func isRouteStatusEqual(statusA, statusB *gwv1.RouteStatus) bool {
	return cmp.Equal(statusA, statusB)
}

// syncStatus updates the status of the Gateway and xRoute CRs
func (s *ProxySyncer) syncStatus(ctx context.Context, rm reports.ReportMap, gwl gwv1.GatewayList) {
	ctx = contextutils.WithLogger(ctx, "statusSyncer")
	logger := contextutils.LoggerFrom(ctx)
	stopwatch := statsutils.NewTranslatorStopWatch("GatewayStatusSyncer")
	stopwatch.Start()
	defer stopwatch.Stop(ctx)

	for _, gw := range gwl.Items {
		gw := gw // pike
		if status := rm.BuildGWStatus(ctx, gw); status != nil {
			if !isGatewayStatusEqual(&gw.Status, status) {
				gw.Status = *status
				if err := s.mgr.GetClient().Status().Patch(ctx, &gw, client.Merge); err != nil {
					logger.Error(err)
				}
			}
		}
	}

	s.syncRouteStatus(ctx, rm)
}

// reconcileProxies persists the proxies that were generated during translations and stores them in an in-memory cache
// This cache is utilized by the debug.ProxyEndpointServer
// As well as to resync the Gloo Xds Translator (when it receives new proxies using a MultiResourceClient)
func (s *ProxySyncer) reconcileProxies(ctx context.Context, proxyList gloo_solo_io.ProxyList) {
	ctx = contextutils.WithLogger(ctx, "proxyCache")
	logger := contextutils.LoggerFrom(ctx)

	// Proxy CR is located in the writeNamespace, which may be different from the originating Gateway CR
	err := s.proxyReconciler.Reconcile(
		s.writeNamespace,
		proxyList,
		func(original, desired *gloo_solo_io.Proxy) (bool, error) {
			// always update
			return true, nil
		},
		clients.ListOpts{
			Ctx:      ctx,
			Selector: kubeGatewayProxyLabels,
		})
	if err != nil {
		// A write error to our cache should not impact translation
		// We will emit a message, and continue
		logger.Error(err)
	}
}

func applyPostTranslationPlugins(ctx context.Context, pluginRegistry registry.PluginRegistry, translationContext *gwplugins.PostTranslationContext) {
	ctx = contextutils.WithLogger(ctx, "postTranslation")
	logger := contextutils.LoggerFrom(ctx)

	for _, postTranslationPlugin := range pluginRegistry.GetPostTranslationPlugins() {
		err := postTranslationPlugin.ApplyPostTranslationPlugin(ctx, translationContext)
		if err != nil {
			logger.Errorf("Error applying post-translation plugin: %v", err)
			continue
		}
	}
}

var opts = cmp.Options{
	cmpopts.IgnoreFields(metav1.Condition{}, "LastTransitionTime"),
	cmpopts.IgnoreMapEntries(func(k string, _ any) bool {
		return k == "lastTransitionTime"
	}),
}

func isGatewayStatusEqual(objA, objB *gwv1.GatewayStatus) bool {
	if cmp.Equal(objA, objB, opts) {
		return true
	}
	return false
}

// compareRouteStatus compares two RouteStatus objects directly
func compareRouteStatus(objA, objB *gwv1.RouteStatus) bool {
	// Use cmp.Equal or any other comparison logic here
	return cmp.Equal(objA, objB, opts)
}
