/*
Copyright AppsCode Inc. and Contributors

Licensed under the AppsCode Community License 1.0.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://github.com/appscode/licenses/raw/1.0.0/AppsCode-Community-1.0.0.md

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	catalog "kubedb.dev/apimachinery/apis/catalog/v1alpha1"
	"kubedb.dev/apimachinery/apis/kubedb"
	api "kubedb.dev/apimachinery/apis/kubedb/v1alpha2"
	cs "kubedb.dev/apimachinery/client/clientset/versioned"
	api_listers "kubedb.dev/apimachinery/client/listers/kubedb/v1alpha2"
	amc "kubedb.dev/apimachinery/pkg/controller"
	"kubedb.dev/apimachinery/pkg/controller/initializer/stash"
	"kubedb.dev/apimachinery/pkg/eventer"

	pcm "github.com/prometheus-operator/prometheus-operator/pkg/client/versioned/typed/monitoring/v1"
	auditlib "go.bytebuilders.dev/audit/lib"
	core "k8s.io/api/core/v1"
	crd_cs "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	reg_util "kmodules.xyz/client-go/admissionregistration/v1"
	"kmodules.xyz/client-go/apiextensions"
	core_util "kmodules.xyz/client-go/core/v1"
	"kmodules.xyz/client-go/discovery"
	meta_util "kmodules.xyz/client-go/meta"
	"kmodules.xyz/client-go/tools/queue"
	appcat "kmodules.xyz/custom-resources/apis/appcatalog/v1alpha1"
	appcat_cs "kmodules.xyz/custom-resources/client/clientset/versioned"
)

type Controller struct {
	amc.Config
	*amc.Controller

	// Prometheus client
	promClient pcm.MonitoringV1Interface
	// LabelSelector to filter Stash restore invokers only for this database
	selector metav1.LabelSelector

	// Redis
	rdQueue    *queue.Worker
	rdInformer cache.SharedIndexInformer
	rdLister   api_listers.RedisLister

	// Redis Sentinel
	rsQueue    *queue.Worker
	rsInformer cache.SharedIndexInformer
	rsLister   api_listers.RedisSentinelLister

	//Redis StatefulSet Informer
	rdStsInformer cache.SharedIndexInformer
}

func New(
	clientConfig *rest.Config,
	client kubernetes.Interface,
	crdClient crd_cs.Interface,
	extClient cs.Interface,
	dynamicClient dynamic.Interface,
	appCatalogClient appcat_cs.Interface,
	promClient pcm.MonitoringV1Interface,
	opt amc.Config,
	topology *core_util.Topology,
	recorder record.EventRecorder,
	mapper discovery.ResourceMapper,
	auditor *auditlib.EventPublisher,
) *Controller {
	return &Controller{
		Controller: &amc.Controller{
			ClientConfig:     clientConfig,
			Client:           client,
			DBClient:         extClient,
			CRDClient:        crdClient,
			DynamicClient:    dynamicClient,
			AppCatalogClient: appCatalogClient,
			ClusterTopology:  topology,
			Recorder:         recorder,
			Mapper:           mapper,
			Auditor:          auditor,
		},
		Config:     opt,
		promClient: promClient,
		selector: metav1.LabelSelector{
			MatchLabels: map[string]string{
				meta_util.NameLabelKey:      api.Redis{}.ResourceFQN(),
				meta_util.ManagedByLabelKey: kubedb.GroupName,
			},
		},
	}
}

// EnsureCustomResourceDefinitions ensures CRD for Redis, DormantDatabase
func (c *Controller) EnsureCustomResourceDefinitions() error {
	klog.Infoln("Ensuring CustomResourceDefinition...")
	crds := []*apiextensions.CustomResourceDefinition{
		api.Redis{}.CustomResourceDefinition(),
		catalog.RedisVersion{}.CustomResourceDefinition(),
		appcat.AppBinding{}.CustomResourceDefinition(),

		api.RedisSentinel{}.CustomResourceDefinition(),
	}
	return apiextensions.RegisterCRDs(c.CRDClient, crds)
}

// InitInformer initializes Redis, DormantDB amd Snapshot watcher
func (c *Controller) Init() error {
	c.initWatcher()
	c.initSecretWatcher()
	c.stsWatcher()

	c.initSentinelWatcher()
	return nil
}

// RunControllers runs queue.worker
func (c *Controller) RunControllers(stopCh <-chan struct{}) {
	// Start Redis controller
	c.rdQueue.Run(stopCh)

	c.rsQueue.Run(stopCh)

	// Start Redis health checker
	c.RunHealthChecker(stopCh)
	c.RunSentinelHealthChecker(stopCh)
}

// Blocks caller. Intended to be called as a Go routine.
func (c *Controller) Run(stopCh <-chan struct{}) {
	go c.StartAndRunControllers(stopCh)
}

// StartAndRunControllers starts InformerFactory and runs queue.worker
func (c *Controller) StartAndRunControllers(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	klog.Infoln("Starting KubeDB controller")
	c.KubeInformerFactory.Start(stopCh)
	c.KubedbInformerFactory.Start(stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	for t, v := range c.KubeInformerFactory.WaitForCacheSync(stopCh) {
		if !v {
			klog.Fatalf("%v timed out waiting for caches to sync", t)
			return
		}
	}
	for t, v := range c.KubedbInformerFactory.WaitForCacheSync(stopCh) {
		if !v {
			klog.Fatalf("%v timed out waiting for caches to sync", t)
			return
		}
	}

	// Start StatefulSet controller
	c.StsQueue.Run(stopCh)

	// Initialize and start Stash controllers
	go stash.NewController(c.Controller, &c.Config.Initializers.Stash, c.RestrictToNamespace).StartAfterStashInstalled(c.MaxNumRequeues, c.NumThreads, c.selector, stopCh)

	// Start Redis controller
	c.RunControllers(stopCh)

	if c.EnableMutatingWebhook {
		cancel1, _ := reg_util.SyncMutatingWebhookCABundle(c.ClientConfig, mutatingWebhookConfig)
		defer cancel1()
	}
	if c.EnableValidatingWebhook {
		cancel2, _ := reg_util.SyncValidatingWebhookCABundle(c.ClientConfig, validatingWebhookConfig)
		defer cancel2()
	}

	<-stopCh
	klog.Infoln("Stopping KubeDB controller")
}

func (c *Controller) pushFailureEvent(db *api.Redis, reason string) {
	c.Recorder.Eventf(
		db,
		core.EventTypeWarning,
		eventer.EventReasonFailedToStart,
		`Fail to be ready Redis: "%v". Reason: %v`,
		db.Name,
		reason,
	)
}
func (c *Controller) pushSentinelFailureEvent(db *api.RedisSentinel, reason string) {
	c.Recorder.Eventf(
		db,
		core.EventTypeWarning,
		eventer.EventReasonFailedToStart,
		`Fail to be ready Redis: "%v". Reason: %v`,
		db.Name,
		reason,
	)
}
