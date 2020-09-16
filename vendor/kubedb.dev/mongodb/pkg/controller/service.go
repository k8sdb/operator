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
	"context"
	"fmt"

	api "kubedb.dev/apimachinery/apis/kubedb/v1alpha1"
	"kubedb.dev/apimachinery/pkg/eventer"

	"github.com/appscode/go/log"
	core "k8s.io/api/core/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kutil "kmodules.xyz/client-go"
	core_util "kmodules.xyz/client-go/core/v1"
	mona "kmodules.xyz/monitoring-agent-api/api/v1"
	ofst "kmodules.xyz/offshoot-api/api/v1"
)

const (
	MongoDBPort = 27017
)

var (
	defaultDBPort = core.ServicePort{
		Name:       "db",
		Protocol:   core.ProtocolTCP,
		Port:       MongoDBPort,
		TargetPort: intstr.FromString("db"),
	}
)

func (c *Controller) ensureService(mongodb *api.MongoDB) (kutil.VerbType, error) {
	// Check if service name exists
	if err := c.checkService(mongodb, mongodb.ServiceName()); err != nil {
		return kutil.VerbUnchanged, err
	}

	// create database Service
	vt, err := c.createService(mongodb)
	if err != nil {
		return kutil.VerbUnchanged, err
	} else if vt != kutil.VerbUnchanged {
		c.recorder.Eventf(
			mongodb,
			core.EventTypeNormal,
			eventer.EventReasonSuccessful,
			"Successfully %s Service",
			vt,
		)
	}
	return vt, nil
}

func (c *Controller) checkService(mongodb *api.MongoDB, serviceName string) error {
	service, err := c.Client.CoreV1().Services(mongodb.Namespace).Get(context.TODO(), serviceName, metav1.GetOptions{})
	if err != nil {
		if kerr.IsNotFound(err) {
			return nil
		}
		return err
	}

	if service.Labels[api.LabelDatabaseKind] != api.ResourceKindMongoDB ||
		service.Labels[api.LabelDatabaseName] != mongodb.Name {
		return fmt.Errorf(`intended service "%v/%v" already exists`, mongodb.Namespace, serviceName)
	}

	return nil
}

func (c *Controller) createService(mongodb *api.MongoDB) (kutil.VerbType, error) {
	meta := metav1.ObjectMeta{
		Name:      mongodb.OffshootName(),
		Namespace: mongodb.Namespace,
	}
	owner := metav1.NewControllerRef(mongodb, api.SchemeGroupVersion.WithKind(api.ResourceKindMongoDB))

	selector := mongodb.OffshootSelectors()
	if mongodb.Spec.ShardTopology != nil {
		selector = mongodb.MongosSelectors()
	}

	_, ok, err := core_util.CreateOrPatchService(
		context.TODO(),
		c.Client,
		meta,
		func(in *core.Service) *core.Service {
			core_util.EnsureOwnerReference(&in.ObjectMeta, owner)
			in.Labels = mongodb.OffshootLabels()
			in.Annotations = mongodb.Spec.ServiceTemplate.Annotations

			in.Spec.Selector = selector
			in.Spec.Ports = ofst.MergeServicePorts(
				core_util.MergeServicePorts(in.Spec.Ports, []core.ServicePort{defaultDBPort}),
				mongodb.Spec.ServiceTemplate.Spec.Ports,
			)

			if mongodb.Spec.ServiceTemplate.Spec.ClusterIP != "" {
				in.Spec.ClusterIP = mongodb.Spec.ServiceTemplate.Spec.ClusterIP
			}
			if mongodb.Spec.ServiceTemplate.Spec.Type != "" {
				in.Spec.Type = mongodb.Spec.ServiceTemplate.Spec.Type
			}
			in.Spec.ExternalIPs = mongodb.Spec.ServiceTemplate.Spec.ExternalIPs
			in.Spec.LoadBalancerIP = mongodb.Spec.ServiceTemplate.Spec.LoadBalancerIP
			in.Spec.LoadBalancerSourceRanges = mongodb.Spec.ServiceTemplate.Spec.LoadBalancerSourceRanges
			in.Spec.ExternalTrafficPolicy = mongodb.Spec.ServiceTemplate.Spec.ExternalTrafficPolicy
			if mongodb.Spec.ServiceTemplate.Spec.HealthCheckNodePort > 0 {
				in.Spec.HealthCheckNodePort = mongodb.Spec.ServiceTemplate.Spec.HealthCheckNodePort
			}
			return in
		},
		metav1.PatchOptions{},
	)
	return ok, err
}

func (c *Controller) ensureStatsService(mongodb *api.MongoDB) (kutil.VerbType, error) {
	// return if monitoring is not prometheus
	if mongodb.GetMonitoringVendor() != mona.VendorPrometheus {
		log.Infoln("spec.monitor.agent is not coreos-operator or builtin.")
		return kutil.VerbUnchanged, nil
	}

	// Check if stats Service name exists
	if err := c.checkService(mongodb, mongodb.StatsService().ServiceName()); err != nil {
		return kutil.VerbUnchanged, err
	}

	owner := metav1.NewControllerRef(mongodb, api.SchemeGroupVersion.WithKind(api.ResourceKindMongoDB))

	// create/patch stats Service
	meta := metav1.ObjectMeta{
		Name:      mongodb.StatsService().ServiceName(),
		Namespace: mongodb.Namespace,
	}
	_, vt, err := core_util.CreateOrPatchService(
		context.TODO(),
		c.Client,
		meta,
		func(in *core.Service) *core.Service {
			core_util.EnsureOwnerReference(&in.ObjectMeta, owner)
			in.Labels = mongodb.StatsServiceLabels()
			in.Spec.Selector = mongodb.OffshootSelectors()
			in.Spec.Ports = core_util.MergeServicePorts(in.Spec.Ports, []core.ServicePort{
				{
					Name:       api.PrometheusExporterPortName,
					Protocol:   core.ProtocolTCP,
					Port:       mongodb.Spec.Monitor.Prometheus.Exporter.Port,
					TargetPort: intstr.FromString(api.PrometheusExporterPortName),
				},
			})
			return in
		},
		metav1.PatchOptions{},
	)
	if err != nil {
		return kutil.VerbUnchanged, err
	} else if vt != kutil.VerbUnchanged {
		c.recorder.Eventf(
			mongodb,
			core.EventTypeNormal,
			eventer.EventReasonSuccessful,
			"Successfully %s stats service",
			vt,
		)
	}
	return vt, nil
}

func (c *Controller) ensureMongoGvrSvc(mongodb *api.MongoDB) error {
	owner := metav1.NewControllerRef(mongodb, api.SchemeGroupVersion.WithKind(api.ResourceKindMongoDB))

	svcFunc := func(svcName string, labels, selectors map[string]string) error {

		// Check if service name exists with different db kind
		if err := c.checkService(mongodb, svcName); err != nil {
			return err
		}

		meta := metav1.ObjectMeta{
			Name:      svcName,
			Namespace: mongodb.Namespace,
		}

		_, vt, err := core_util.CreateOrPatchService(
			context.TODO(),
			c.Client,
			meta,
			func(in *core.Service) *core.Service {
				core_util.EnsureOwnerReference(&in.ObjectMeta, owner)
				in.Labels = labels
				// 'tolerate-unready-endpoints' annotation is deprecated.
				// Use: spec.PublishNotReadyAddresses
				// ref: https://github.com/kubernetes/kubernetes/pull/63742.
				// TODO: delete this annotation
				in.Annotations = map[string]string{
					"service.alpha.kubernetes.io/tolerate-unready-endpoints": "true",
				}
				in.Spec.Selector = selectors
				in.Spec.Type = core.ServiceTypeClusterIP
				in.Spec.ClusterIP = core.ClusterIPNone
				in.Spec.PublishNotReadyAddresses = true
				in.Spec.Ports = []core.ServicePort{
					{
						Name: "db",
						Port: MongoDBPort,
					},
				}
				return in
			},
			metav1.PatchOptions{},
		)

		if err == nil {
			c.recorder.Eventf(
				mongodb,
				core.EventTypeNormal,
				eventer.EventReasonSuccessful,
				"Successfully %s stats service",
				vt,
			)
		}
		return err
	}

	if mongodb.Spec.ShardTopology != nil {
		topology := mongodb.Spec.ShardTopology
		// create shard governing service
		for i := int32(0); i < topology.Shard.Shards; i++ {
			if err := svcFunc(mongodb.GvrSvcName(
				mongodb.ShardNodeName(i)),
				mongodb.ShardLabels(i),
				mongodb.ShardSelectors(i),
			); err != nil {
				return err
			}
		}
		// create configsvr governing service
		if err := svcFunc(mongodb.GvrSvcName(
			mongodb.ConfigSvrNodeName()),
			mongodb.ConfigSvrLabels(),
			mongodb.ConfigSvrSelectors(),
		); err != nil {
			return err
		}

		// create mongos governing service
		return svcFunc(mongodb.GvrSvcName(
			mongodb.MongosNodeName()),
			mongodb.MongosLabels(),
			mongodb.MongosSelectors(),
		)
	}
	// create mongodb governing service
	return svcFunc(mongodb.GvrSvcName(
		mongodb.OffshootName()),
		mongodb.OffshootLabels(),
		mongodb.OffshootSelectors(),
	)
}
