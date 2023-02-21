package framework

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	storagev1alpha1 "k8s.io/api/storage/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/events"
	schedconfig "k8s.io/kubernetes/cmd/kube-scheduler/app/config"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/scheduler"
	kubeschedulerconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultpreemption"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/volumebinding"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/profile"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/k-cloud-labs/kluster-capacity/pkg"
	"github.com/k-cloud-labs/kluster-capacity/pkg/plugins/generic"
	"github.com/k-cloud-labs/kluster-capacity/pkg/utils"
)

func init() {
	if err := corev1.AddToScheme(legacyscheme.Scheme); err != nil {
		fmt.Printf("err: %v\n", err)
	}
	// add your own scheme here to use dynamic informer factory when you have some custom filter plugins
	// which uses other resources than defined in scheduler.
	// for details, refer to k8s.io/kubernetes/pkg/scheduler/eventhandlers.go
}

var (
	initResources = map[schema.GroupVersionKind]func() runtime.Object{
		corev1.SchemeGroupVersion.WithKind("Namespace"):             func() runtime.Object { return &corev1.Namespace{} },
		corev1.SchemeGroupVersion.WithKind("Pod"):                   func() runtime.Object { return &corev1.Pod{} },
		corev1.SchemeGroupVersion.WithKind("Node"):                  func() runtime.Object { return &corev1.Node{} },
		corev1.SchemeGroupVersion.WithKind("PersistentVolume"):      func() runtime.Object { return &corev1.PersistentVolume{} },
		corev1.SchemeGroupVersion.WithKind("PersistentVolumeClaim"): func() runtime.Object { return &corev1.PersistentVolumeClaim{} },
		storagev1.SchemeGroupVersion.WithKind("StorageClass"):       func() runtime.Object { return &storagev1.StorageClass{} },
		storagev1.SchemeGroupVersion.WithKind("CSINode"):            func() runtime.Object { return &storagev1.CSINode{} },
		storagev1.SchemeGroupVersion.WithKind("CSIDriver"):          func() runtime.Object { return &storagev1.CSIDriver{} },
		storagev1.SchemeGroupVersion.WithKind("CSIStorageCapacity"): func() runtime.Object { return &storagev1alpha1.CSIStorageCapacity{} },
	}
	once        sync.Once
	initObjects []runtime.Object
)

type genericSimulator struct {
	// fake clientset used by scheduler
	fakeClient clientset.Interface
	// fake informer factory used by scheduler
	fakeInformerFactory informers.SharedInformerFactory
	restMapper          meta.RESTMapper
	// real dynamic client to init the world
	dynamicClient dynamic.Interface

	// scheduler
	scheduler                *scheduler.Scheduler
	excludeNodes             sets.String
	withScheduledPods        bool
	withNodeImages           bool
	ignorePodsOnExcludesNode bool
	outOfTreeRegistry        frameworkruntime.Registry
	customBind               kubeschedulerconfig.PluginSet
	customPreBind            kubeschedulerconfig.PluginSet
	customPostBind           kubeschedulerconfig.PluginSet
	customEventHandlers      []func()
	postBindHook             func(*corev1.Pod) error

	// for scheduler and informer
	informerCh  chan struct{}
	schedulerCh chan struct{}

	// for simulator
	stopCh  chan struct{}
	stopMux sync.Mutex
	stopped bool

	// final status
	status pkg.Status
	// save status to this file if specified
	saveTo string
}

type Option func(*genericSimulator)

func WithExcludeNodes(excludeNodes []string) Option {
	return func(s *genericSimulator) {
		s.excludeNodes = sets.NewString(excludeNodes...)
	}
}

func WithOutOfTreeRegistry(registry frameworkruntime.Registry) Option {
	return func(s *genericSimulator) {
		s.outOfTreeRegistry = registry
	}
}

func WithCustomBind(plugins kubeschedulerconfig.PluginSet) Option {
	return func(s *genericSimulator) {
		s.customBind = plugins
	}
}

func WithCustomPreBind(plugins kubeschedulerconfig.PluginSet) Option {
	return func(s *genericSimulator) {
		s.customPreBind = plugins
	}
}

func WithCustomPostBind(plugins kubeschedulerconfig.PluginSet) Option {
	return func(s *genericSimulator) {
		s.customPostBind = plugins
	}
}

func WithCustomEventHandlers(handlers []func()) Option {
	return func(s *genericSimulator) {
		s.customEventHandlers = handlers
	}
}

func WithNodeImages(with bool) Option {
	return func(s *genericSimulator) {
		s.withNodeImages = with
	}
}

func WithScheduledPods(with bool) Option {
	return func(s *genericSimulator) {
		s.withScheduledPods = with
	}
}

func WithIgnorePodsOnExcludesNode(with bool) Option {
	return func(s *genericSimulator) {
		s.ignorePodsOnExcludesNode = with
	}
}

func WithPostBindHook(postBindHook func(*corev1.Pod) error) Option {
	return func(s *genericSimulator) {
		s.postBindHook = postBindHook
	}
}

func WithSaveTo(to string) Option {
	return func(s *genericSimulator) {
		s.saveTo = to
	}
}

// NewGenericSimulator create a generic simulator for ce, cc, ss simulator which is completely independent of apiserver so no need
// for kubeconfig nor for apiserver url
func NewGenericSimulator(kubeSchedulerConfig *schedconfig.CompletedConfig, restConfig *restclient.Config, options ...Option) (pkg.Simulator, error) {
	kubeSchedulerConfig.InformerFactory.InformerFor(&corev1.Pod{}, newPodInformer)

	dynamicClient := dynamic.NewForConfigOrDie(restConfig)
	restMapper, err := apiutil.NewDynamicRESTMapper(restConfig)
	if err != nil {
		return nil, err
	}

	s := &genericSimulator{
		fakeClient:               kubeSchedulerConfig.Client,
		dynamicClient:            dynamicClient,
		restMapper:               restMapper,
		stopCh:                   make(chan struct{}),
		fakeInformerFactory:      kubeSchedulerConfig.InformerFactory,
		informerCh:               make(chan struct{}),
		schedulerCh:              make(chan struct{}),
		withScheduledPods:        true,
		ignorePodsOnExcludesNode: false,
		withNodeImages:           true,
	}
	for _, option := range options {
		option(s)
	}

	scheduler, err := s.createScheduler(kubeSchedulerConfig)
	if err != nil {
		return nil, err
	}

	s.scheduler = scheduler

	s.fakeInformerFactory.Start(s.informerCh)

	return s, nil
}

func (s *genericSimulator) GetPodsByNode(nodeName string) ([]*corev1.Pod, error) {
	dump := s.scheduler.SchedulerCache.Dump()
	var res []*corev1.Pod
	if dump != nil && dump.Nodes[nodeName] != nil {
		podInfos := dump.Nodes[nodeName].Pods
		for i := range podInfos {
			if podInfos[i].Pod != nil {
				res = append(res, podInfos[i].Pod)
			}
		}
	}

	if res == nil {
		return nil, errors.New("cannot get pods on the node because dump is nil")
	}
	return res, nil
}

// InitTheWorld use objs outside or default init resources to initialize the scheduler
// the objs outside must be typed object.
func (s *genericSimulator) InitTheWorld(objs ...runtime.Object) error {
	if len(objs) == 0 {
		// black magic
		initObjects := getInitObjects(s.restMapper, s.dynamicClient)
		for _, unstructuredObj := range initObjects {
			obj := initResources[unstructuredObj.GetObjectKind().GroupVersionKind()]()
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.(*unstructured.Unstructured).UnstructuredContent(), obj); err != nil {
				return err
			}
			if needAdd, obj := s.preAdd(obj); needAdd {
				if err := s.fakeClient.(*fake.Clientset).Tracker().Add(obj); err != nil {
					return err
				}
			}
		}
	} else {
		for _, obj := range objs {
			if _, ok := obj.(runtime.Unstructured); ok {
				return errors.New("type of objs used to init the world must not be unstructured")
			}
			if needAdd, obj := s.preAdd(obj); needAdd {
				if err := s.fakeClient.(*fake.Clientset).Tracker().Add(obj); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (s *genericSimulator) UpdateScheduledPods(pod ...*corev1.Pod) {
	s.status.Pods = append(s.status.Pods, pod...)
}

func (s *genericSimulator) UpdateNodesToScaleDown(nodeName string) {
	s.status.NodesToScaleDown = append(s.status.NodesToScaleDown, nodeName)
}

func (s *genericSimulator) Status() pkg.Status {
	return s.status
}

func (s *genericSimulator) Stop(reason string) error {
	nodeMap := make(map[string]corev1.Node)
	nodeList, _ := s.fakeClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{ResourceVersion: "0"})
	for _, node := range nodeList.Items {
		nodeMap[node.Name] = node
	}

	s.stopMux.Lock()
	defer func() {
		s.stopMux.Unlock()
	}()

	if s.stopped {
		return nil
	}

	if len(s.saveTo) > 0 {
		file, err := os.OpenFile(s.saveTo, os.O_CREATE|os.O_RDWR, 0755)
		if err != nil {
			panic(err)
		}
		defer file.Close()

		bytes, err := json.Marshal(s.status)
		if err != nil {
			return err
		}

		_, err = file.Write(bytes)
		if err != nil {
			return err
		}
	}

	defer func() {
		close(s.stopCh)
		close(s.informerCh)
		close(s.schedulerCh)
	}()

	s.status.StopReason = reason
	s.status.Nodes = nodeMap
	s.stopped = true

	return nil
}

func (s *genericSimulator) CreatePod(pod *corev1.Pod) error {
	_, err := s.fakeClient.CoreV1().Pods(pod.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	return err
}

func (s *genericSimulator) Run() error {
	// wait for all informer cache synced
	s.fakeInformerFactory.WaitForCacheSync(s.informerCh)

	go s.scheduler.Run(context.TODO())

	<-s.stopCh

	return nil
}

func (s *genericSimulator) createScheduler(cc *schedconfig.CompletedConfig) (*scheduler.Scheduler, error) {
	// custom event handlers
	for _, handler := range s.customEventHandlers {
		handler()
	}

	// register default generic plugin
	if s.outOfTreeRegistry == nil {
		s.outOfTreeRegistry = make(frameworkruntime.Registry)
	}
	err := s.outOfTreeRegistry.Register(generic.Name, func(configuration runtime.Object, f framework.Handle) (framework.Plugin, error) {
		return generic.New(s.postBindHook, s.fakeClient)
	})
	if err != nil {
		return nil, err
	}

	if cc.ComponentConfig.Profiles[0].Plugins.PreBind == nil {
		cc.ComponentConfig.Profiles[0].Plugins.PreBind = &kubeschedulerconfig.PluginSet{}
	}
	if cc.ComponentConfig.Profiles[0].Plugins.Bind == nil {
		cc.ComponentConfig.Profiles[0].Plugins.Bind = &kubeschedulerconfig.PluginSet{}
	}
	if cc.ComponentConfig.Profiles[0].Plugins.PostBind == nil {
		cc.ComponentConfig.Profiles[0].Plugins.PostBind = &kubeschedulerconfig.PluginSet{}
	}

	cc.ComponentConfig.Profiles[0].Plugins.PreBind.Enabled = append(cc.ComponentConfig.Profiles[0].Plugins.PreBind.Enabled, kubeschedulerconfig.Plugin{Name: generic.Name})
	cc.ComponentConfig.Profiles[0].Plugins.PreBind.Disabled = append(cc.ComponentConfig.Profiles[0].Plugins.PreBind.Disabled, kubeschedulerconfig.Plugin{Name: volumebinding.Name})
	cc.ComponentConfig.Profiles[0].Plugins.Bind.Enabled = append(cc.ComponentConfig.Profiles[0].Plugins.Bind.Enabled, kubeschedulerconfig.Plugin{Name: generic.Name})
	cc.ComponentConfig.Profiles[0].Plugins.Bind.Disabled = append(cc.ComponentConfig.Profiles[0].Plugins.Bind.Disabled, kubeschedulerconfig.Plugin{Name: defaultbinder.Name})
	cc.ComponentConfig.Profiles[0].Plugins.PostBind.Enabled = append(cc.ComponentConfig.Profiles[0].Plugins.PostBind.Enabled, kubeschedulerconfig.Plugin{Name: generic.Name})
	cc.ComponentConfig.Profiles[0].Plugins.PostBind.Disabled = append(cc.ComponentConfig.Profiles[0].Plugins.PostBind.Disabled, kubeschedulerconfig.Plugin{Name: defaultpreemption.Name})

	// custom bind plugin
	cc.ComponentConfig.Profiles[0].Plugins.PreBind.Enabled = append(cc.ComponentConfig.Profiles[0].Plugins.PreBind.Enabled, s.customPreBind.Enabled...)
	cc.ComponentConfig.Profiles[0].Plugins.PreBind.Disabled = append(cc.ComponentConfig.Profiles[0].Plugins.PreBind.Disabled, s.customPreBind.Disabled...)
	cc.ComponentConfig.Profiles[0].Plugins.Bind.Enabled = append(cc.ComponentConfig.Profiles[0].Plugins.Bind.Enabled, s.customBind.Enabled...)
	cc.ComponentConfig.Profiles[0].Plugins.Bind.Disabled = append(cc.ComponentConfig.Profiles[0].Plugins.Bind.Disabled, s.customBind.Disabled...)
	cc.ComponentConfig.Profiles[0].Plugins.PostBind.Enabled = append(cc.ComponentConfig.Profiles[0].Plugins.PostBind.Enabled, s.customPostBind.Enabled...)
	cc.ComponentConfig.Profiles[0].Plugins.PostBind.Disabled = append(cc.ComponentConfig.Profiles[0].Plugins.PostBind.Disabled, s.customPostBind.Disabled...)

	// create the scheduler.
	return scheduler.New(
		s.fakeClient,
		s.fakeInformerFactory,
		getRecorderFactory(cc),
		s.schedulerCh,
		scheduler.WithProfiles(cc.ComponentConfig.Profiles...),
		scheduler.WithPercentageOfNodesToScore(cc.ComponentConfig.PercentageOfNodesToScore),
		scheduler.WithFrameworkOutOfTreeRegistry(s.outOfTreeRegistry),
		scheduler.WithPodMaxBackoffSeconds(cc.ComponentConfig.PodMaxBackoffSeconds),
		scheduler.WithPodInitialBackoffSeconds(cc.ComponentConfig.PodInitialBackoffSeconds),
		scheduler.WithExtenders(cc.ComponentConfig.Extenders...),
		scheduler.WithParallelism(cc.ComponentConfig.Parallelism),
	)
}

func (s *genericSimulator) preAdd(obj runtime.Object) (bool, runtime.Object) {
	// filter exclude nodes and pods and update pod, node spec and status property
	if pod, ok := obj.(*corev1.Pod); ok {
		// ignore ds pods on exclude nodes
		if s.excludeNodes != nil {
			if _, ok := s.excludeNodes[pod.Spec.NodeName]; ok {
				if s.ignorePodsOnExcludesNode || pod.OwnerReferences != nil && pod.OwnerReferences[0].Kind == "DaemonSet" {
					return false, nil
				}
			}
		}

		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed || pod.DeletionTimestamp != nil {
			return false, nil
		}
		if !s.withScheduledPods {
			pod := utils.InitPod(pod)
			pod.Status.Phase = corev1.PodPending

			return true, pod
		}
	} else if node, ok := obj.(*corev1.Node); ok && s.excludeNodes != nil {
		if _, ok := s.excludeNodes[node.Name]; ok {
			return false, nil
		} else if !s.withNodeImages {
			node.Status.Images = nil

			return true, node
		}
	}

	return true, obj
}

func newPodInformer(cs clientset.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	selector := fmt.Sprintf("status.phase!=%v,status.phase!=%v", corev1.PodSucceeded, corev1.PodFailed)
	tweakListOptions := func(options *metav1.ListOptions) {
		options.FieldSelector = selector
	}
	return coreinformers.NewFilteredPodInformer(cs, metav1.NamespaceAll, resyncPeriod, nil, tweakListOptions)
}

func getRecorderFactory(cc *schedconfig.CompletedConfig) profile.RecorderFactory {
	return func(name string) events.EventRecorder {
		return cc.EventBroadcaster.NewRecorder(name)
	}
}

// getInitObjects return all objects need to add to scheduler.
// it's pkg scope for multi scheduler to avoid calling too much times of real kube-apiserver
func getInitObjects(restMapper meta.RESTMapper, dynClient dynamic.Interface) []runtime.Object {
	once.Do(func() {
		// each item is UnstructuredList
		for gvk := range initResources {
			restMapping, err := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
			if err != nil && !meta.IsNoMatchError(err) {
				fmt.Printf("unable to get rest mapping for %s, error: %s", gvk.String(), err.Error())
				os.Exit(1)
			}

			if restMapping != nil {
				var (
					list *unstructured.UnstructuredList
					err  error
				)
				if restMapping.Scope.Name() == meta.RESTScopeNameRoot {
					list, err = dynClient.Resource(restMapping.Resource).List(context.TODO(), metav1.ListOptions{ResourceVersion: "0"})
					if err != nil && !apierrors.IsNotFound(err) {
						fmt.Printf("unable to list %s, error: %s", gvk.String(), err.Error())
						os.Exit(1)
					}
				} else {
					list, err = dynClient.Resource(restMapping.Resource).Namespace(metav1.NamespaceAll).List(context.TODO(), metav1.ListOptions{ResourceVersion: "0"})
					if err != nil && !apierrors.IsNotFound(err) {
						fmt.Printf("unable to list %s, error: %s", gvk.String(), err.Error())
						os.Exit(1)
					}
				}

				_ = list.EachListItem(func(object runtime.Object) error {
					initObjects = append(initObjects, object)
					return nil
				})
			}
		}
	})

	return initObjects
}
