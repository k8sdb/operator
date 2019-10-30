/*
Copyright The KubeDB Authors.

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
package admission

import (
	"fmt"
	"sync"

	api "kubedb.dev/apimachinery/apis/kubedb/v1alpha1"
	cs "kubedb.dev/apimachinery/client/clientset/versioned"

	"github.com/appscode/go/log"
	"github.com/appscode/go/types"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	admission "k8s.io/api/admission/v1beta1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	kutil "kmodules.xyz/client-go"
	core_util "kmodules.xyz/client-go/core/v1"
	meta_util "kmodules.xyz/client-go/meta"
	mona "kmodules.xyz/monitoring-agent-api/api/v1"
	hookapi "kmodules.xyz/webhook-runtime/admission/v1beta1"
)

type MySQLMutator struct {
	client      kubernetes.Interface
	extClient   cs.Interface
	lock        sync.RWMutex
	initialized bool
}

var _ hookapi.AdmissionHook = &MySQLMutator{}

func (a *MySQLMutator) Resource() (plural schema.GroupVersionResource, singular string) {
	return schema.GroupVersionResource{
			Group:    "mutators.kubedb.com",
			Version:  "v1alpha1",
			Resource: "mysqlmutators",
		},
		"mysqlmutator"
}

func (a *MySQLMutator) Initialize(config *rest.Config, stopCh <-chan struct{}) error {
	a.lock.Lock()
	defer a.lock.Unlock()

	a.initialized = true

	var err error
	if a.client, err = kubernetes.NewForConfig(config); err != nil {
		return err
	}
	if a.extClient, err = cs.NewForConfig(config); err != nil {
		return err
	}
	return err
}

func (a *MySQLMutator) Admit(req *admission.AdmissionRequest) *admission.AdmissionResponse {
	status := &admission.AdmissionResponse{}

	// N.B.: No Mutating for delete
	if (req.Operation != admission.Create && req.Operation != admission.Update) ||
		len(req.SubResource) != 0 ||
		req.Kind.Group != api.SchemeGroupVersion.Group ||
		req.Kind.Kind != api.ResourceKindMySQL {
		status.Allowed = true
		return status
	}

	a.lock.RLock()
	defer a.lock.RUnlock()
	if !a.initialized {
		return hookapi.StatusUninitialized()
	}
	obj, err := meta_util.UnmarshalFromJSON(req.Object.Raw, api.SchemeGroupVersion)
	if err != nil {
		return hookapi.StatusBadRequest(err)
	}
	mysqlMod, err := setDefaultValues(a.extClient, obj.(*api.MySQL).DeepCopy())
	if err != nil {
		return hookapi.StatusForbidden(err)
	} else if mysqlMod != nil {
		patch, err := meta_util.CreateJSONPatch(req.Object.Raw, mysqlMod)
		if err != nil {
			return hookapi.StatusInternalServerError(err)
		}
		status.Patch = patch
		patchType := admission.PatchTypeJSONPatch
		status.PatchType = &patchType
	}

	status.Allowed = true
	return status
}

// setDefaultValues provides the defaulting that is performed in mutating stage of creating/updating a MySQL database
func setDefaultValues(extClient cs.Interface, mysql *api.MySQL) (runtime.Object, error) {
	if mysql.Spec.Version == "" {
		return nil, errors.New(`'spec.version' is missing`)
	}

	if mysql.Spec.Topology != nil && mysql.Spec.Topology.Mode != nil &&
		*mysql.Spec.Topology.Mode == api.MySQLClusterModeGroup {
		if mysql.Spec.Topology.Group == nil {
			mysql.Spec.Topology.Group = &api.MySQLGroupSpec{}
		}

		if mysql.Spec.Topology.Group.Name == "" {
			grName, err := uuid.NewRandom()
			if err != nil {
				return nil, errors.New("failed to generate a new group name")
			}
			mysql.Spec.Topology.Group.Name = grName.String()
		}

		if mysql.Spec.Topology.Group.BaseServerID == nil {
			mysql.Spec.Topology.Group.BaseServerID = types.UIntP(api.MySQLDefaultBaseServerID)
		}
	}

	mysql.SetDefaults()

	if err := setDefaultsFromDormantDB(extClient, mysql); err != nil {
		return nil, err
	}

	// If monitoring spec is given without port,
	// set default Listening port
	setMonitoringPort(mysql)

	return mysql, nil
}

// setDefaultsFromDormantDB takes values from Similar Dormant Database
func setDefaultsFromDormantDB(extClient cs.Interface, mysql *api.MySQL) error {
	// Check if DormantDatabase exists or not
	dormantDb, err := extClient.KubedbV1alpha1().DormantDatabases(mysql.Namespace).Get(mysql.Name, metav1.GetOptions{})
	if err != nil {
		if !kerr.IsNotFound(err) {
			return err
		}
		return nil
	}

	// Check DatabaseKind
	if value, _ := meta_util.GetStringValue(dormantDb.Labels, api.LabelDatabaseKind); value != api.ResourceKindMySQL {
		return errors.New(fmt.Sprintf(`invalid MySQL: "%v/%v". Exists DormantDatabase "%v/%v" of different Kind`, mysql.Namespace, mysql.Name, dormantDb.Namespace, dormantDb.Name))
	}

	// Check Origin Spec
	ddbOriginSpec := dormantDb.Spec.Origin.Spec.MySQL
	ddbOriginSpec.SetDefaults()

	// If DatabaseSecret of new object is not given,
	// Take dormantDatabaseSecretName
	if mysql.Spec.DatabaseSecret == nil {
		mysql.Spec.DatabaseSecret = ddbOriginSpec.DatabaseSecret
	}

	// If Monitoring Spec of new object is not given,
	// Take Monitoring Settings from Dormant
	if mysql.Spec.Monitor == nil {
		mysql.Spec.Monitor = ddbOriginSpec.Monitor
	} else {
		ddbOriginSpec.Monitor = mysql.Spec.Monitor
	}

	// If Backup Scheduler of new object is not given,
	// Take Backup Scheduler Settings from Dormant
	if mysql.Spec.BackupSchedule == nil {
		mysql.Spec.BackupSchedule = ddbOriginSpec.BackupSchedule
	} else {
		ddbOriginSpec.BackupSchedule = mysql.Spec.BackupSchedule
	}

	// Skip checking UpdateStrategy
	ddbOriginSpec.UpdateStrategy = mysql.Spec.UpdateStrategy

	// Skip checking ServiceAccountName
	ddbOriginSpec.PodTemplate.Spec.ServiceAccountName = mysql.Spec.PodTemplate.Spec.ServiceAccountName

	// Skip checking TerminationPolicy
	ddbOriginSpec.TerminationPolicy = mysql.Spec.TerminationPolicy

	if !meta_util.Equal(ddbOriginSpec, &mysql.Spec) {
		diff := meta_util.Diff(ddbOriginSpec, &mysql.Spec)
		log.Errorf("mysql spec mismatches with OriginSpec in DormantDatabases. Diff: %v", diff)
		return errors.New(fmt.Sprintf("mysql spec mismatches with OriginSpec in DormantDatabases. Diff: %v", diff))
	}

	if _, err := meta_util.GetString(mysql.Annotations, api.AnnotationInitialized); err == kutil.ErrNotFound &&
		mysql.Spec.Init != nil &&
		(mysql.Spec.Init.SnapshotSource != nil || mysql.Spec.Init.StashRestoreSession != nil) {
		mysql.Annotations = core_util.UpsertMap(mysql.Annotations, map[string]string{
			api.AnnotationInitialized: "",
		})
	}

	// Delete  Matching dormantDatabase in Controller

	return nil
}

// Assign Default Monitoring Port if MonitoringSpec Exists
// and the AgentVendor is Prometheus.
func setMonitoringPort(mysql *api.MySQL) {
	if mysql.Spec.Monitor != nil &&
		mysql.GetMonitoringVendor() == mona.VendorPrometheus {
		if mysql.Spec.Monitor.Prometheus == nil {
			mysql.Spec.Monitor.Prometheus = &mona.PrometheusSpec{}
		}
		if mysql.Spec.Monitor.Prometheus.Port == 0 {
			mysql.Spec.Monitor.Prometheus.Port = api.PrometheusExporterPortNumber
		}
	}
}
