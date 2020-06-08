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
	"context"
	"fmt"
	"strings"
	"sync"

	"kubedb.dev/apimachinery/apis/kubedb"
	api "kubedb.dev/apimachinery/apis/kubedb/v1alpha1"
	cs "kubedb.dev/apimachinery/client/clientset/versioned"
	amv "kubedb.dev/apimachinery/pkg/validator"

	"github.com/pkg/errors"
	admission "k8s.io/api/admission/v1beta1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	core_util "kmodules.xyz/client-go/core/v1"
	meta_util "kmodules.xyz/client-go/meta"
	hookapi "kmodules.xyz/webhook-runtime/admission/v1beta1"
)

type MongoDBValidator struct {
	ClusterTopology *core_util.Topology

	client      kubernetes.Interface
	extClient   cs.Interface
	lock        sync.RWMutex
	initialized bool
}

var _ hookapi.AdmissionHook = &MongoDBValidator{}

var forbiddenEnvVars = []string{
	"MONGO_INITDB_ROOT_USERNAME",
	"MONGO_INITDB_ROOT_PASSWORD",
}

func (a *MongoDBValidator) Resource() (plural schema.GroupVersionResource, singular string) {
	return schema.GroupVersionResource{
			Group:    kubedb.ValidatorGroupName,
			Version:  "v1alpha1",
			Resource: api.ResourcePluralMongoDB,
		},
		api.ResourceSingularMongoDB
}

func (a *MongoDBValidator) Initialize(config *rest.Config, stopCh <-chan struct{}) error {
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

func (a *MongoDBValidator) Admit(req *admission.AdmissionRequest) *admission.AdmissionResponse {
	status := &admission.AdmissionResponse{}

	if (req.Operation != admission.Create && req.Operation != admission.Update && req.Operation != admission.Delete) ||
		len(req.SubResource) != 0 ||
		req.Kind.Group != api.SchemeGroupVersion.Group ||
		req.Kind.Kind != api.ResourceKindMongoDB {
		status.Allowed = true
		return status
	}

	a.lock.RLock()
	defer a.lock.RUnlock()
	if !a.initialized {
		return hookapi.StatusUninitialized()
	}

	switch req.Operation {
	case admission.Delete:
		if req.Name != "" {
			// req.Object.Raw = nil, so read from kubernetes
			obj, err := a.extClient.KubedbV1alpha1().MongoDBs(req.Namespace).Get(context.TODO(), req.Name, metav1.GetOptions{})
			if err != nil && !kerr.IsNotFound(err) {
				return hookapi.StatusInternalServerError(err)
			} else if err == nil && obj.Spec.TerminationPolicy == api.TerminationPolicyDoNotTerminate {
				return hookapi.StatusBadRequest(fmt.Errorf(`mongodb "%v/%v" can't be terminated. To delete, change spec.terminationPolicy`, req.Namespace, req.Name))
			}
		}
	default:
		obj, err := meta_util.UnmarshalFromJSON(req.Object.Raw, api.SchemeGroupVersion)
		if err != nil {
			return hookapi.StatusBadRequest(err)
		}
		if req.Operation == admission.Update {
			// validate changes made by user
			oldObject, err := meta_util.UnmarshalFromJSON(req.OldObject.Raw, api.SchemeGroupVersion)
			if err != nil {
				return hookapi.StatusBadRequest(err)
			}

			mongodb := obj.(*api.MongoDB).DeepCopy()
			oldMongoDB := oldObject.(*api.MongoDB).DeepCopy()
			mgVersion, err := getMongoDBVersion(a.extClient, oldMongoDB.Spec.Version)
			if err != nil {
				return hookapi.StatusInternalServerError(err)
			}
			oldMongoDB.SetDefaults(mgVersion, a.ClusterTopology)
			// Allow changing Database Secret only if there was no secret have set up yet.
			if oldMongoDB.Spec.DatabaseSecret == nil {
				oldMongoDB.Spec.DatabaseSecret = mongodb.Spec.DatabaseSecret
			}

			if err := validateUpdate(mongodb, oldMongoDB); err != nil {
				return hookapi.StatusBadRequest(fmt.Errorf("%v", err))
			}
		}
		// validate database specs
		if err = ValidateMongoDB(a.client, a.extClient, obj.(*api.MongoDB), false); err != nil {
			return hookapi.StatusForbidden(err)
		}
	}
	status.Allowed = true
	return status
}

// ValidateMongoDB checks if the object satisfies all the requirements.
// It is not method of Interface, because it is referenced from controller package too.
func ValidateMongoDB(client kubernetes.Interface, extClient cs.Interface, mongodb *api.MongoDB, strictValidation bool) error {
	if mongodb.Spec.Version == "" {
		return errors.New(`'spec.version' is missing`)
	}
	if _, err := extClient.CatalogV1alpha1().MongoDBVersions().Get(context.TODO(), string(mongodb.Spec.Version), metav1.GetOptions{}); err != nil {
		return err
	}

	top := mongodb.Spec.ShardTopology
	if top != nil {
		if mongodb.Spec.Replicas != nil {
			return fmt.Errorf(`doesn't support 'spec.replicas' when spec.shardTopology is set`)
		}
		if mongodb.Spec.PodTemplate != nil {
			return fmt.Errorf(`doesn't support 'spec.podTemplate' when spec.shardTopology is set`)
		}
		if mongodb.Spec.ConfigSource != nil {
			return fmt.Errorf(`doesn't support 'spec.configSource' when spec.shardTopology is set`)
		}

		// Validate Topology Replicas values
		if top.Shard.Shards < 1 {
			return fmt.Errorf(`spec.shardTopology.shard.shards %v invalid. Must be greater than zero when spec.shardTopology is set`, top.Shard.Shards)
		}
		if top.Shard.Replicas < 1 {
			return fmt.Errorf(`spec.shardTopology.shard.replicas %v invalid. Must be greater than zero when spec.shardTopology is set`, top.Shard.Replicas)
		}
		if top.ConfigServer.Replicas < 1 {
			return fmt.Errorf(`spec.shardTopology.configServer.replicas %v invalid. Must be greater than zero when spec.shardTopology is set`, top.ConfigServer.Replicas)
		}
		if top.Mongos.Replicas < 1 {
			return fmt.Errorf(`spec.shardTopology.mongos.replicas %v invalid. Must be greater than zero when spec.shardTopology is set`, top.Mongos.Replicas)
		}

		// Validate Mongos deployment strategy
		if top.Mongos.Strategy.Type == "" {
			return fmt.Errorf(`spec.shardTopology.mongos.strategy.type is missing`)
		}

		// Validate Envs
		if err := amv.ValidateEnvVar(top.Shard.PodTemplate.Spec.Env, forbiddenEnvVars, api.ResourceKindMongoDB); err != nil {
			return err
		}
		if err := amv.ValidateEnvVar(top.ConfigServer.PodTemplate.Spec.Env, forbiddenEnvVars, api.ResourceKindMongoDB); err != nil {
			return err
		}
		if err := amv.ValidateEnvVar(top.Mongos.PodTemplate.Spec.Env, forbiddenEnvVars, api.ResourceKindMongoDB); err != nil {
			return err
		}
	} else {
		if mongodb.Spec.Replicas == nil || *mongodb.Spec.Replicas < 1 {
			return fmt.Errorf(`spec.replicas "%v" invalid. Must be greater than zero in non-shardTopology`, mongodb.Spec.Replicas)
		}

		if mongodb.Spec.Replicas == nil || (mongodb.Spec.ReplicaSet == nil && *mongodb.Spec.Replicas != 1) {
			return fmt.Errorf(`spec.replicas "%v" invalid for 'MongoDB Standalone' instance. Value must be one`, mongodb.Spec.Replicas)
		}

		if mongodb.Spec.PodTemplate != nil {
			if err := amv.ValidateEnvVar(mongodb.Spec.PodTemplate.Spec.Env, forbiddenEnvVars, api.ResourceKindMongoDB); err != nil {
				return err
			}
		}
	}

	if mongodb.Spec.StorageType == "" {
		return fmt.Errorf(`'spec.storageType' is missing`)
	}
	if mongodb.Spec.StorageType != api.StorageTypeDurable && mongodb.Spec.StorageType != api.StorageTypeEphemeral {
		return fmt.Errorf(`'spec.storageType' %s is invalid`, mongodb.Spec.StorageType)
	}
	// Validate storage for ClusterTopology or non-ClusterTopology
	if top != nil {
		if mongodb.Spec.Storage != nil {
			return fmt.Errorf("doesn't support 'spec.storage' when spec.shardTopology is set")
		}
		if err := amv.ValidateStorage(client, mongodb.Spec.StorageType, top.Shard.Storage, "spec.shardTopology.shard.storage"); err != nil {
			return err
		}
		if err := amv.ValidateStorage(client, mongodb.Spec.StorageType, top.ConfigServer.Storage, "spec.shardTopology.configServer.storage"); err != nil {
			return err
		}
	} else {
		if err := amv.ValidateStorage(client, mongodb.Spec.StorageType, mongodb.Spec.Storage); err != nil {
			return err
		}
	}

	if (mongodb.Spec.ClusterAuthMode == api.ClusterAuthModeX509 || mongodb.Spec.ClusterAuthMode == api.ClusterAuthModeSendX509) &&
		(mongodb.Spec.SSLMode == api.SSLModeDisabled || mongodb.Spec.SSLMode == api.SSLModeAllowSSL) {
		return fmt.Errorf("can't have %v set to mongodb.spec.sslMode when mongodb.spec.clusterAuthMode is set to %v",
			mongodb.Spec.SSLMode, mongodb.Spec.ClusterAuthMode)
	}

	if mongodb.Spec.ClusterAuthMode == api.ClusterAuthModeSendKeyFile && mongodb.Spec.SSLMode == api.SSLModeDisabled {
		return fmt.Errorf("can't have %v set to mongodb.spec.sslMode when mongodb.spec.clusterAuthMode is set to %v",
			mongodb.Spec.SSLMode, mongodb.Spec.ClusterAuthMode)
	}

	if strictValidation {
		if mongodb.Spec.DatabaseSecret != nil {
			if _, err := client.CoreV1().Secrets(mongodb.Namespace).Get(context.TODO(), mongodb.Spec.DatabaseSecret.SecretName, metav1.GetOptions{}); err != nil {
				return err
			}
		}

		if mongodb.Spec.KeyFile != nil {
			if _, err := client.CoreV1().Secrets(mongodb.Namespace).Get(context.TODO(), mongodb.Spec.KeyFile.SecretName, metav1.GetOptions{}); err != nil {
				return err
			}
		}

		// Check if mongodbVersion is deprecated.
		// If deprecated, return error
		mongodbVersion, err := extClient.CatalogV1alpha1().MongoDBVersions().Get(context.TODO(), string(mongodb.Spec.Version), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if mongodbVersion.Spec.Deprecated {
			return fmt.Errorf("mongoDB %s/%s is using deprecated version %v. Skipped processing",
				mongodb.Namespace, mongodb.Name, mongodbVersion.Name)
		}

		if err := mongodbVersion.ValidateSpecs(); err != nil {
			return fmt.Errorf("mongodb %s/%s is using invalid mongodbVersion %v. Skipped processing. reason: %v", mongodb.Namespace,
				mongodb.Name, mongodbVersion.Name, err)
		}
	}

	if mongodb.Spec.UpdateStrategy.Type == "" {
		return fmt.Errorf(`'spec.updateStrategy.type' is missing`)
	}

	if mongodb.Spec.TerminationPolicy == "" {
		return fmt.Errorf(`'spec.terminationPolicy' is missing`)
	}

	if mongodb.Spec.StorageType == api.StorageTypeEphemeral && mongodb.Spec.TerminationPolicy == api.TerminationPolicyHalt {
		return fmt.Errorf(`'spec.terminationPolicy: Halt' can not be used for 'Ephemeral' storage`)
	}

	monitorSpec := mongodb.Spec.Monitor
	if monitorSpec != nil {
		if err := amv.ValidateMonitorSpec(monitorSpec); err != nil {
			return err
		}
	}

	return nil
}

func validateUpdate(obj, oldObj runtime.Object) error {
	preconditions := getPreconditionFunc()
	_, err := meta_util.CreateStrategicPatch(oldObj, obj, preconditions...)
	if err != nil {
		if mergepatch.IsPreconditionFailed(err) {
			return fmt.Errorf("%v.%v", err, preconditionFailedError())
		}
		return err
	}
	return nil
}

func getPreconditionFunc() []mergepatch.PreconditionFunc {
	preconditions := []mergepatch.PreconditionFunc{
		mergepatch.RequireKeyUnchanged("apiVersion"),
		mergepatch.RequireKeyUnchanged("kind"),
		mergepatch.RequireMetadataKeyUnchanged("name"),
		mergepatch.RequireMetadataKeyUnchanged("namespace"),
	}

	for _, field := range preconditionSpecFields {
		preconditions = append(preconditions,
			meta_util.RequireChainKeyUnchanged(field),
		)
	}
	return preconditions
}

var preconditionSpecFields = []string{
	"spec.storageType",
	"spec.storage",
	"spec.databaseSecret",
	"spec.certificateSecret",
	"spec.init",
	"spec.replicaSet.name",
	"spec.shardTopology.*.storage",
	"spec.shardTopology.*.prefix",
}

func preconditionFailedError() error {
	str := preconditionSpecFields
	strList := strings.Join(str, "\n\t")
	return fmt.Errorf(strings.Join([]string{`At least one of the following was changed:
	apiVersion
	kind
	name
	namespace`, strList}, "\n\t"))
}
