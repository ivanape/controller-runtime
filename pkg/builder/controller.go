/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package builder

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/cluster"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// Supporting mocking out functions for testing.
var newController = controller.NewUnmanaged
var getGvk = apiutil.GVKForObject

// project represents other forms that we can use to
// send/receive a given resource (metadata-only, unstructured, etc).
type objectProjection int

const (
	// projectAsNormal doesn't change the object from the form given.
	projectAsNormal objectProjection = iota
	// projectAsMetadata turns this into a metadata-only watch.
	projectAsMetadata
)

var _ controller.ClusterWatcher = &clusterWatcher{}

// clusterWatcher sets up watches between a cluster and a controller.
type clusterWatcher struct {
	ctrl                   controller.Controller
	forInput               ForInput
	ownsInput              []OwnsInput
	watchesInput           []WatchesInput
	globalPredicates       []predicate.Predicate
	clusterAwareRawSources []source.ClusterAwareSource
}

// Builder builds a Controller.
type Builder struct {
	clusterWatcher
	rawSources  []source.Source
	mgr         manager.Manager
	ctrlOptions controller.Options
	name        string
}

// ControllerManagedBy returns a new controller builder that will be started by the provided Manager.
func ControllerManagedBy(m manager.Manager) *Builder {
	return &Builder{mgr: m}
}

// ForInput represents the information set by the For method.
type ForInput struct {
	object           client.Object
	predicates       []predicate.Predicate
	objectProjection objectProjection
	err              error
}

// For defines the type of Object being *reconciled*, and configures the ControllerManagedBy to respond to create / delete /
// update events by *reconciling the object*.
// This is the equivalent of calling
// Watches(&source.Kind{Type: apiType}, &handler.EnqueueRequestForObject{}).
func (blder *Builder) For(object client.Object, opts ...ForOption) *Builder {
	if blder.forInput.object != nil {
		blder.forInput.err = fmt.Errorf("For(...) should only be called once, could not assign multiple objects for reconciliation")
		return blder
	}
	input := ForInput{object: object}
	for _, opt := range opts {
		opt.ApplyToFor(&input)
	}

	blder.forInput = input
	return blder
}

// OwnsInput represents the information set by Owns method.
type OwnsInput struct {
	matchEveryOwner  bool
	object           client.Object
	predicates       []predicate.Predicate
	objectProjection objectProjection
}

// Owns defines types of Objects being *generated* by the ControllerManagedBy, and configures the ControllerManagedBy to respond to
// create / delete / update events by *reconciling the owner object*.
//
// The default behavior reconciles only the first controller-type OwnerReference of the given type.
// Use Owns(object, builder.MatchEveryOwner) to reconcile all owners.
//
// By default, this is the equivalent of calling
// Watches(object, handler.EnqueueRequestForOwner([...], ownerType, OnlyControllerOwner())).
func (blder *Builder) Owns(object client.Object, opts ...OwnsOption) *Builder {
	input := OwnsInput{object: object}
	for _, opt := range opts {
		opt.ApplyToOwns(&input)
	}

	blder.ownsInput = append(blder.ownsInput, input)
	return blder
}

// WatchesInput represents the information set by Watches method.
type WatchesInput struct {
	obj              client.Object
	handler          handler.EventHandler
	predicates       []predicate.Predicate
	objectProjection objectProjection
}

// Watches defines the type of Object to watch, and configures the ControllerManagedBy to respond to create / delete /
// update events by *reconciling the object* with the given EventHandler.
//
// This is the equivalent of calling
// WatchesRawSource(source.Kind(cache, object, eventHandler, predicates...)).
func (blder *Builder) Watches(object client.Object, eventHandler handler.EventHandler, opts ...WatchesOption) *Builder {
	input := WatchesInput{
		obj:     object,
		handler: eventHandler,
	}
	for _, opt := range opts {
		opt.ApplyToWatches(&input)
	}

	blder.watchesInput = append(blder.watchesInput, input)
	return blder
}

// WatchesMetadata is the same as Watches, but forces the internal cache to only watch PartialObjectMetadata.
//
// This is useful when watching lots of objects, really big objects, or objects for which you only know
// the GVK, but not the structure. You'll need to pass metav1.PartialObjectMetadata to the client
// when fetching objects in your reconciler, otherwise you'll end up with a duplicate structured or unstructured cache.
//
// When watching a resource with metadata only, for example the v1.Pod, you should not Get and List using the v1.Pod type.
// Instead, you should use the special metav1.PartialObjectMetadata type.
//
// ❌ Incorrect:
//
//	pod := &v1.Pod{}
//	mgr.GetClient().Get(ctx, nsAndName, pod)
//
// ✅ Correct:
//
//	pod := &metav1.PartialObjectMetadata{}
//	pod.SetGroupVersionKind(schema.GroupVersionKind{
//	    Group:   "",
//	    Version: "v1",
//	    Kind:    "Pod",
//	})
//	mgr.GetClient().Get(ctx, nsAndName, pod)
//
// In the first case, controller-runtime will create another cache for the
// concrete type on top of the metadata cache; this increases memory
// consumption and leads to race conditions as caches are not in sync.
func (blder *Builder) WatchesMetadata(object client.Object, eventHandler handler.EventHandler, opts ...WatchesOption) *Builder {
	opts = append(opts, OnlyMetadata)
	return blder.Watches(object, eventHandler, opts...)
}

// WatchesRawSource exposes the lower-level ControllerManagedBy Watches functions through the builder.
// Specified predicates are registered only for given source.
//
// STOP! Consider using For(...), Owns(...), Watches(...), WatchesMetadata(...) instead.
// This method is only exposed for more advanced use cases, most users should use one of the higher level functions.
//
// WatchesRawSource does not respect predicates configured through WithEventFilter.
func (blder *Builder) WatchesRawSource(src source.Source) *Builder {
	if src, ok := src.(source.ClusterAwareSource); ok {
		blder.clusterAwareRawSources = append(blder.clusterAwareRawSources, src)
		return blder
	}

	blder.rawSources = append(blder.rawSources, src)
	return blder
}

// WithEventFilter sets the event filters, to filter which create/update/delete/generic events eventually
// trigger reconciliations. For example, filtering on whether the resource version has changed.
// Given predicate is added for all watched objects.
// Defaults to the empty list.
func (blder *Builder) WithEventFilter(p predicate.Predicate) *Builder {
	blder.globalPredicates = append(blder.globalPredicates, p)
	return blder
}

// WithOptions overrides the controller options used in doController. Defaults to empty.
func (blder *Builder) WithOptions(options controller.Options) *Builder {
	blder.ctrlOptions = options
	return blder
}

// WithLogConstructor overrides the controller options's LogConstructor.
func (blder *Builder) WithLogConstructor(logConstructor func(*reconcile.Request) logr.Logger) *Builder {
	blder.ctrlOptions.LogConstructor = logConstructor
	return blder
}

// Named sets the name of the controller to the given name. The name shows up
// in metrics, among other things, and thus should be a prometheus compatible name
// (underscores and alphanumeric characters only).
//
// By default, controllers are named using the lowercase version of their kind.
func (blder *Builder) Named(name string) *Builder {
	blder.name = name
	return blder
}

// Complete builds the Application Controller.
func (blder *Builder) Complete(r reconcile.Reconciler) error {
	_, err := blder.Build(r)
	return err
}

// Build builds the Application Controller and returns the Controller it created.
func (blder *Builder) Build(r reconcile.Reconciler) (controller.Controller, error) {
	if r == nil {
		return nil, fmt.Errorf("must provide a non-nil Reconciler")
	}
	if blder.mgr == nil {
		return nil, fmt.Errorf("must provide a non-nil Manager")
	}
	if blder.forInput.err != nil {
		return nil, blder.forInput.err
	}

	// Set the ControllerManagedBy
	if err := blder.doController(r); err != nil {
		return nil, err
	}

	// Set the Watch
	if err := blder.doWatch(); err != nil {
		return nil, err
	}

	ctrl := blder.ctrl
	if *blder.ctrlOptions.EngageWithProviderClusters {
		// wrap as cluster.Aware to be engaged with provider clusters on demand
		ctrl = controller.NewMultiClusterController(ctrl, &blder.clusterWatcher)
	}
	if err := blder.mgr.Add(ctrl); err != nil {
		return nil, err
	}

	return blder.ctrl, nil
}

func project(cl cluster.Cluster, obj client.Object, proj objectProjection) (client.Object, error) {
	switch proj {
	case projectAsNormal:
		return obj, nil
	case projectAsMetadata:
		metaObj := &metav1.PartialObjectMetadata{}
		gvk, err := getGvk(obj, cl.GetScheme())
		if err != nil {
			return nil, fmt.Errorf("unable to determine GVK of %T for a metadata-only watch: %w", obj, err)
		}
		metaObj.SetGroupVersionKind(gvk)
		return metaObj, nil
	default:
		panic(fmt.Sprintf("unexpected projection type %v on type %T, should not be possible since this is an internal field", proj, obj))
	}
}

func (cc *clusterWatcher) Watch(ctx context.Context, cl cluster.Cluster) error {
	// Reconcile type
	if cc.forInput.object != nil {
		obj, err := project(cl, cc.forInput.object, cc.forInput.objectProjection)
		if err != nil {
			return err
		}
		hdler := &handler.EnqueueRequestForObject{}
		allPredicates := append([]predicate.Predicate(nil), cc.globalPredicates...)
		allPredicates = append(allPredicates, cc.forInput.predicates...)
		src := &ctxBoundedSyncingSource{ctx: ctx, src: source.Kind(cl.GetCache(), obj, handler.ForCluster(cl.Name(), hdler), allPredicates...)}
		if err := cc.ctrl.Watch(src); err != nil {
			return err
		}
	}

	// Watches the managed types
	for _, own := range cc.ownsInput {
		obj, err := project(cl, own.object, own.objectProjection)
		if err != nil {
			return err
		}
		opts := []handler.OwnerOption{}
		if !own.matchEveryOwner {
			opts = append(opts, handler.OnlyControllerOwner())
		}
		hdler := handler.EnqueueRequestForOwner(
			cl.GetScheme(), cl.GetRESTMapper(),
			cc.forInput.object,
			opts...,
		)

		allPredicates := append([]predicate.Predicate(nil), cc.globalPredicates...)
		allPredicates = append(allPredicates, own.predicates...)
		src := &ctxBoundedSyncingSource{ctx: ctx, src: source.Kind(cl.GetCache(), obj, handler.ForCluster(cl.Name(), hdler), allPredicates...)}
		if err := cc.ctrl.Watch(src); err != nil {
			return err
		}
	}

	// Watches extra types
	for _, w := range cc.watchesInput {
		projected, err := project(cl, w.obj, w.objectProjection)
		if err != nil {
			return fmt.Errorf("failed to project for %T: %w", w.obj, err)
		}
		allPredicates := append([]predicate.Predicate(nil), cc.globalPredicates...)
		allPredicates = append(allPredicates, w.predicates...)
		src := &ctxBoundedSyncingSource{ctx: ctx, src: source.Kind(cl.GetCache(), projected, handler.ForCluster(cl.Name(), w.handler), allPredicates...)}
		if err := cc.ctrl.Watch(src); err != nil {
			return err
		}
	}

	for _, src := range cc.clusterAwareRawSources {
		if err := cc.ctrl.Watch(src); err != nil {
			return err
		}
	}

	return nil
}

func (blder *Builder) doWatch() error {
	// Pre-checks for a valid configuration
	if len(blder.ownsInput) > 0 && blder.forInput.object == nil {
		return errors.New("Owns() can only be used together with For()")
	}
	if len(blder.watchesInput) == 0 && blder.forInput.object == nil && len(blder.rawSources) == 0 {
		return errors.New("there are no watches configured, controller will never get triggered. Use For(), Owns(), Watches() or WatchesRawSource() to set them up")
	}
	if !*blder.ctrlOptions.EngageWithDefaultCluster && len(blder.rawSources) > 0 {
		return errors.New("when using a cluster adapter without watching the default cluster, non-cluster-aware custom raw watches are not allowed")
	}

	if *blder.ctrlOptions.EngageWithDefaultCluster {
		if err := blder.Watch(unboundedContext, blder.mgr); err != nil {
			return err
		}

		for _, src := range blder.rawSources {
			if err := blder.ctrl.Watch(src); err != nil {
				return err
			}
		}
	}
	return nil
}

func (blder *Builder) getControllerName(gvk schema.GroupVersionKind, hasGVK bool) (string, error) {
	if blder.name != "" {
		return blder.name, nil
	}
	if !hasGVK {
		return "", errors.New("one of For() or Named() must be called")
	}
	return strings.ToLower(gvk.Kind), nil
}

func (blder *Builder) doController(r reconcile.Reconciler) error {
	globalOpts := blder.mgr.GetControllerOptions()

	if blder.ctrlOptions.Reconciler != nil && r != nil {
		return errors.New("reconciler was set via WithOptions() and via Build() or Complete()")
	}
	if blder.ctrlOptions.Reconciler == nil {
		blder.ctrlOptions.Reconciler = r
	}

	// Retrieve the GVK from the object we're reconciling
	// to pre-populate logger information, and to optionally generate a default name.
	var gvk schema.GroupVersionKind
	hasGVK := blder.forInput.object != nil
	if hasGVK {
		var err error
		gvk, err = getGvk(blder.forInput.object, blder.mgr.GetScheme())
		if err != nil {
			return err
		}
	}

	// Setup concurrency.
	if blder.ctrlOptions.MaxConcurrentReconciles == 0 && hasGVK {
		groupKind := gvk.GroupKind().String()

		if concurrency, ok := globalOpts.GroupKindConcurrency[groupKind]; ok && concurrency > 0 {
			blder.ctrlOptions.MaxConcurrentReconciles = concurrency
		}
	}

	// Setup cache sync timeout.
	if blder.ctrlOptions.CacheSyncTimeout == 0 && globalOpts.CacheSyncTimeout > 0 {
		blder.ctrlOptions.CacheSyncTimeout = globalOpts.CacheSyncTimeout
	}

	controllerName, err := blder.getControllerName(gvk, hasGVK)
	if err != nil {
		return err
	}

	// Setup the logger.
	if blder.ctrlOptions.LogConstructor == nil {
		log := blder.mgr.GetLogger().WithValues(
			"controller", controllerName,
		)
		if hasGVK {
			log = log.WithValues(
				"controllerGroup", gvk.Group,
				"controllerKind", gvk.Kind,
			)
		}

		blder.ctrlOptions.LogConstructor = func(req *reconcile.Request) logr.Logger {
			log := log
			if req != nil {
				if hasGVK {
					log = log.WithValues(gvk.Kind, klog.KRef(req.Namespace, req.Name))
				}
				log = log.WithValues(
					"namespace", req.Namespace, "name", req.Name,
				)
			}
			return log
		}
	}

	// Default which clusters to engage with.
	if blder.ctrlOptions.EngageWithDefaultCluster == nil {
		blder.ctrlOptions.EngageWithDefaultCluster = globalOpts.EngageWithDefaultCluster
	}
	if blder.ctrlOptions.EngageWithProviderClusters == nil {
		blder.ctrlOptions.EngageWithProviderClusters = globalOpts.EngageWithProviderClusters
	}
	if blder.ctrlOptions.EngageWithDefaultCluster == nil {
		return errors.New("EngageWithDefaultCluster must not be nil") // should not happen due to defaulting
	}
	if blder.ctrlOptions.EngageWithProviderClusters == nil {
		return errors.New("EngageWithProviderClusters must not be nil") // should not happen due to defaulting
	}
	if !*blder.ctrlOptions.EngageWithDefaultCluster && !*blder.ctrlOptions.EngageWithProviderClusters {
		return errors.New("EngageWithDefaultCluster and EngageWithProviderClusters are both false, controller will never get triggered")
	}

	// Build the controller and return.
	blder.ctrl, err = newController(controllerName, blder.mgr, blder.ctrlOptions)
	return err
}

// ctxBoundedSyncingSource implements source.SyncingSource and wraps the ctx
// passed to the methods into the life-cycle of another context, i.e. stop
// whenever one of the contexts is done.
type ctxBoundedSyncingSource struct {
	ctx context.Context
	src source.SyncingSource
}

var unboundedContext context.Context = nil //nolint:revive // keep nil explicit for clarity.

var _ source.SyncingSource = &ctxBoundedSyncingSource{}

func (s *ctxBoundedSyncingSource) Start(ctx context.Context, q workqueue.RateLimitingInterface) error {
	return s.src.Start(joinContexts(ctx, s.ctx), q)
}

func (s *ctxBoundedSyncingSource) WaitForSync(ctx context.Context) error {
	return s.src.WaitForSync(joinContexts(ctx, s.ctx))
}

func joinContexts(ctx, bound context.Context) context.Context {
	if bound == unboundedContext {
		return ctx
	}

	ctx, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		<-bound.Done()
	}()
	return ctx
}
