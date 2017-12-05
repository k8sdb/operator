package controller

import (
	"reflect"
	"time"

	"github.com/appscode/go/hold"
	"github.com/appscode/go/log"
	apiext_util "github.com/appscode/kutil/apiextensions/v1beta1"
	pcm "github.com/coreos/prometheus-operator/pkg/client/monitoring/v1"
	api "github.com/kubedb/apimachinery/apis/kubedb/v1alpha1"
	cs "github.com/kubedb/apimachinery/client/typed/kubedb/v1alpha1"
	"github.com/kubedb/apimachinery/client/typed/kubedb/v1alpha1/util"
	amc "github.com/kubedb/apimachinery/pkg/controller"
	"github.com/kubedb/apimachinery/pkg/eventer"
	core "k8s.io/api/core/v1"
	crd_api "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apiext_cs "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
)

type Options struct {
	// Operator namespace
	OperatorNamespace string
	// Exporter tag
	ExporterTag string
	// Governing service
	GoverningService string
	// Address to listen on for web interface and telemetry.
	Address string
	// Enable RBAC for database workloads
	EnableRbac bool
}

type Controller struct {
	*amc.Controller
	// Api Extension Client
	ApiExtKubeClient apiext_cs.ApiextensionsV1beta1Interface
	// Prometheus client
	promClient pcm.MonitoringV1Interface
	// Cron Controller
	cronController amc.CronControllerInterface
	// Event Recorder
	recorder record.EventRecorder
	// Flag data
	opt Options
	// sync time to sync the list.
	syncPeriod time.Duration
}

var _ amc.Deleter = &Controller{}

func New(
	client kubernetes.Interface,
	apiExtKubeClient apiext_cs.ApiextensionsV1beta1Interface,
	extClient cs.KubedbV1alpha1Interface,
	promClient pcm.MonitoringV1Interface,
	cronController amc.CronControllerInterface,
	opt Options,
) *Controller {
	return &Controller{
		Controller: &amc.Controller{
			Client:    client,
			ExtClient: extClient,
		},
		ApiExtKubeClient: apiExtKubeClient,
		promClient:       promClient,
		cronController:   cronController,
		recorder:         eventer.NewEventRecorder(client, "Memcached operator"),
		opt:              opt,
		syncPeriod:       time.Minute * 2,
	}
}

func (c *Controller) Setup() error {
	log.Infoln("Ensuring CustomResourceDefinition...")
	crds := []*crd_api.CustomResourceDefinition{
		api.Memcached{}.CustomResourceDefinition(),
		api.DormantDatabase{}.CustomResourceDefinition(),
	}
	return apiext_util.RegisterCRDs(c.ApiExtKubeClient, crds)
}

func (c *Controller) Run() {

	// Watch x  TPR objects
	go c.watchMemcached()
	// Watch DeletedDatabase with labelSelector only for Memcached
	go c.watchDeletedDatabase()
}

// Blocks caller. Intended to be called as a Go routine.
func (c *Controller) RunAndHold() {
	c.Run()

	// Run HTTP server to expose metrics, audit endpoint & debug profiles.
	go c.runHTTPServer()
	// hold
	hold.Hold()
}

func (c *Controller) watchMemcached() {
	lw := &cache.ListWatch{
		ListFunc: func(opts metav1.ListOptions) (runtime.Object, error) {
			return c.ExtClient.Memcacheds(metav1.NamespaceAll).List(metav1.ListOptions{})
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return c.ExtClient.Memcacheds(metav1.NamespaceAll).Watch(metav1.ListOptions{})
		},
	}

	_, cacheController := cache.NewInformer(
		lw,
		&api.Memcached{},
		c.syncPeriod,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				memcached := obj.(*api.Memcached)
				util.AssignTypeKind(memcached)
				setMonitoringPort(memcached)
				if memcached.Status.CreationTime == nil {
					if err := c.create(memcached); err != nil {
						log.Errorln(err)
						c.pushFailureEvent(memcached, err.Error())
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				memcached := obj.(*api.Memcached)
				util.AssignTypeKind(memcached)
				setMonitoringPort(memcached)
				if err := c.pause(memcached); err != nil {
					log.Errorln(err)
				}
			},
			UpdateFunc: func(old, new interface{}) {
				oldObj, ok := old.(*api.Memcached)
				if !ok {
					return
				}
				newObj, ok := new.(*api.Memcached)
				if !ok {
					return
				}
				util.AssignTypeKind(oldObj)
				util.AssignTypeKind(newObj)
				setMonitoringPort(oldObj)
				setMonitoringPort(newObj)
				if !reflect.DeepEqual(oldObj.Spec, newObj.Spec) {
					if err := c.update(oldObj, newObj); err != nil {
						log.Errorln(err)
					}
				}
			},
		},
	)
	cacheController.Run(wait.NeverStop)
}

func setMonitoringPort(memcached *api.Memcached) {
	if memcached.Spec.Monitor != nil &&
		memcached.Spec.Monitor.Prometheus != nil {
		if memcached.Spec.Monitor.Prometheus.Port == 0 {
			memcached.Spec.Monitor.Prometheus.Port = api.PrometheusExporterPortNumber
		}
	}
}

func (c *Controller) watchDeletedDatabase() {
	labelMap := map[string]string{
		api.LabelDatabaseKind: api.ResourceKindMemcached,
	}
	// Watch with label selector
	lw := &cache.ListWatch{
		ListFunc: func(opts metav1.ListOptions) (runtime.Object, error) {
			return c.ExtClient.DormantDatabases(metav1.NamespaceAll).List(
				metav1.ListOptions{
					LabelSelector: labels.SelectorFromSet(labelMap).String(),
				})
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return c.ExtClient.DormantDatabases(metav1.NamespaceAll).Watch(
				metav1.ListOptions{
					LabelSelector: labels.SelectorFromSet(labelMap).String(),
				})
		},
	}

	amc.NewDormantDbController(c.Client, c.ApiExtKubeClient, c.ExtClient, c, lw, c.syncPeriod).Run()
}

func (c *Controller) pushFailureEvent(memcached *api.Memcached, reason string) {
	c.recorder.Eventf(
		memcached.ObjectReference(),
		core.EventTypeWarning,
		eventer.EventReasonFailedToStart,
		`Fail to be ready Memcached: "%v". Reason: %v`,
		memcached.Name,
		reason,
	)

	_, err := util.TryPatchMemcached(c.ExtClient, memcached.ObjectMeta, func(in *api.Memcached) *api.Memcached {
		in.Status.Phase = api.DatabasePhaseFailed
		in.Status.Reason = reason
		return in
	})
	if err != nil {
		c.recorder.Eventf(memcached.ObjectReference(), core.EventTypeWarning, eventer.EventReasonFailedToUpdate, err.Error())
	}
}
