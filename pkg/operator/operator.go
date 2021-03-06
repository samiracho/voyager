package operator

import (
	"fmt"
	"sync"

	"github.com/appscode/go/log"
	apiext_util "github.com/appscode/kutil/apiextensions/v1beta1"
	"github.com/appscode/kutil/tools/queue"
	api "github.com/appscode/voyager/apis/voyager/v1beta1"
	cs "github.com/appscode/voyager/client"
	voyagerinformers "github.com/appscode/voyager/informers/externalversions"
	api_listers "github.com/appscode/voyager/listers/voyager/v1beta1"
	"github.com/appscode/voyager/pkg/config"
	"github.com/appscode/voyager/pkg/eventer"
	prom "github.com/coreos/prometheus-operator/pkg/client/monitoring/v1"
	kext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	kext_cs "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	apps_listers "k8s.io/client-go/listers/apps/v1beta1"
	core_listers "k8s.io/client-go/listers/core/v1"
	ext_listers "k8s.io/client-go/listers/extensions/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

type Operator struct {
	KubeClient    kubernetes.Interface
	CRDClient     kext_cs.ApiextensionsV1beta1Interface
	VoyagerClient cs.Interface
	PromClient    prom.MonitoringV1Interface
	options       config.Options

	kubeInformerFactory    informers.SharedInformerFactory
	voyagerInformerFactory voyagerinformers.SharedInformerFactory

	recorder record.EventRecorder
	sync.Mutex

	// Certificate CRD
	crtQueue    *queue.Worker
	crtInformer cache.SharedIndexInformer
	crtLister   api_listers.CertificateLister

	// ConfigMap
	cfgQueue    *queue.Worker
	cfgInformer cache.SharedIndexInformer
	cfgLister   core_listers.ConfigMapLister

	// Deployment
	dpQueue    *queue.Worker
	dpInformer cache.SharedIndexInformer
	dpLister   apps_listers.DeploymentLister

	// Endpoint
	epQueue    *queue.Worker
	epInformer cache.SharedIndexInformer
	epLister   core_listers.EndpointsLister

	// Ingress CRD
	engQueue    *queue.Worker
	engInformer cache.SharedIndexInformer
	engLister   api_listers.IngressLister

	// Ingress
	ingQueue    *queue.Worker
	ingInformer cache.SharedIndexInformer
	ingLister   ext_listers.IngressLister

	// Namespace
	nsQueue    *queue.Worker
	nsInformer cache.SharedIndexInformer
	nsLister   core_listers.NamespaceLister

	// Node
	// nodeQueue    *queue.Worker
	nodeInformer cache.SharedIndexInformer
	nodeLister   core_listers.NodeLister

	// Secret
	secretQueue    *queue.Worker
	secretInformer cache.SharedIndexInformer
	secretLister   core_listers.SecretLister

	// Service Monitor
	smonQueue    *queue.Worker
	smonInformer cache.SharedIndexInformer
	// monLister   prom.ServiceMonitorLister

	// Service
	svcQueue    *queue.Worker
	svcInformer cache.SharedIndexInformer
	svcLister   core_listers.ServiceLister
}

func New(
	kubeClient kubernetes.Interface,
	crdClient kext_cs.ApiextensionsV1beta1Interface,
	voyagerClient cs.Interface,
	promClient prom.MonitoringV1Interface,
	opt config.Options,
) *Operator {
	return &Operator{
		KubeClient:             kubeClient,
		kubeInformerFactory:    informers.NewFilteredSharedInformerFactory(kubeClient, opt.ResyncPeriod, opt.WatchNamespace(), nil),
		CRDClient:              crdClient,
		VoyagerClient:          voyagerClient,
		voyagerInformerFactory: voyagerinformers.NewFilteredSharedInformerFactory(voyagerClient, opt.ResyncPeriod, opt.WatchNamespace(), nil),
		PromClient:             promClient,
		options:                opt,
		recorder:               eventer.NewEventRecorder(kubeClient, "voyager operator"),
	}
}

func (op *Operator) Setup() error {
	if err := op.ensureCustomResourceDefinitions(); err != nil {
		return err
	}

	op.initIngressCRDWatcher()
	op.initIngressWatcher()
	op.initDeploymentWatcher()
	op.initServiceWatcher()
	op.initConfigMapWatcher()
	op.initEndpointWatcher()
	op.initSecretWatcher()
	op.initNodeWatcher()
	op.initServiceMonitorWatcher()
	op.initNamespaceWatcher()
	op.initCertificateCRDWatcher()

	return nil
}

func (op *Operator) ensureCustomResourceDefinitions() error {
	log.Infoln("Ensuring CRD registration")

	crds := []*kext.CustomResourceDefinition{
		api.Ingress{}.CustomResourceDefinition(),
		api.Certificate{}.CustomResourceDefinition(),
	}
	return apiext_util.RegisterCRDs(op.CRDClient, crds)
}

func (op *Operator) Run(stopCh chan struct{}) {
	defer runtime.HandleCrash()

	go op.CheckCertificates()

	log.Infoln("Starting Voyager controller")
	op.kubeInformerFactory.Start(stopCh)
	op.voyagerInformerFactory.Start(stopCh)
	if op.smonInformer != nil {
		op.smonInformer.Run(stopCh)
	}

	// Wait for all involved caches to be synced, before processing items from the queue is started
	for _, v := range op.kubeInformerFactory.WaitForCacheSync(stopCh) {
		if !v {
			runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
			return
		}
	}
	for _, v := range op.voyagerInformerFactory.WaitForCacheSync(stopCh) {
		if !v {
			runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
			return
		}
	}
	if op.smonInformer != nil {
		if !cache.WaitForCacheSync(stopCh, op.smonInformer.HasSynced) {
			runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
			return
		}
	}

	op.engQueue.Run(stopCh)
	op.ingQueue.Run(stopCh)
	op.dpQueue.Run(stopCh)
	op.svcQueue.Run(stopCh)
	op.cfgQueue.Run(stopCh)
	op.epQueue.Run(stopCh)
	op.secretQueue.Run(stopCh)
	op.nsQueue.Run(stopCh)
	op.crtQueue.Run(stopCh)
	if op.smonInformer != nil {
		op.smonQueue.Run(stopCh)
	}

	<-stopCh
	log.Infoln("Stopping Stash controller")
}

func (op *Operator) listIngresses() ([]api.Ingress, error) {
	ingList, err := op.ingLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	engList, err := op.engLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	items := make([]api.Ingress, len(engList))
	for i, item := range engList {
		items[i] = *item
	}
	for _, item := range ingList {
		if e, err := api.NewEngressFromIngress(item); err == nil {
			items = append(items, *e)
		}
	}
	return items, nil
}
