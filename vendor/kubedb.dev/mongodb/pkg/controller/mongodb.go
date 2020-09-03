/*
Copyright AppsCode Inc. and Contributors

Licensed under the PolyForm Noncommercial License 1.0.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://github.com/appscode/licenses/raw/1.0.0/PolyForm-Noncommercial-1.0.0.md

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
	"kubedb.dev/apimachinery/client/clientset/versioned/typed/kubedb/v1alpha1/util"
	"kubedb.dev/apimachinery/pkg/eventer"
	validator "kubedb.dev/mongodb/pkg/admission"

	"github.com/appscode/go/log"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kutil "kmodules.xyz/client-go"
	dynamic_util "kmodules.xyz/client-go/dynamic"
	meta_util "kmodules.xyz/client-go/meta"
)

func (c *Controller) create(mongodb *api.MongoDB) error {
	if err := validator.ValidateMongoDB(c.Client, c.ExtClient, mongodb, true); err != nil {
		c.recorder.Event(
			mongodb,
			core.EventTypeWarning,
			eventer.EventReasonInvalid,
			err.Error(),
		)
		log.Errorln(err)
		return nil
	}

	if mongodb.Status.Phase == "" {
		mg, err := util.UpdateMongoDBStatus(context.TODO(), c.ExtClient.KubedbV1alpha1(), mongodb.ObjectMeta, func(in *api.MongoDBStatus) *api.MongoDBStatus {
			in.Phase = api.DatabasePhaseCreating
			return in
		}, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		mongodb.Status = mg.Status
	}

	// create Governing Service
	if err := c.ensureMongoGvrSvc(mongodb); err != nil {
		return fmt.Errorf(`failed to create governing Service for "%v/%v". Reason: %v`, mongodb.Namespace, mongodb.Name, err)
	}

	// Ensure Service account, role, rolebinding, and PSP for database statefulsets
	if err := c.ensureDatabaseRBAC(mongodb); err != nil {
		return err
	}

	// ensure database Service
	vt1, err := c.ensureService(mongodb)
	if err != nil {
		return err
	}

	if err := c.ensureDatabaseSecret(mongodb); err != nil {
		return err
	}

	// ensure certificate or keyfile for cluster
	sslMode := mongodb.Spec.SSLMode
	if (sslMode != api.SSLModeDisabled && sslMode != "") ||
		mongodb.Spec.ReplicaSet != nil || mongodb.Spec.ShardTopology != nil {
		if err := c.ensureKeyFileSecret(mongodb); err != nil {
			return err
		}
	}

	// wait for certificates
	if mongodb.Spec.TLS != nil {
		var secrets []string
		if mongodb.Spec.ShardTopology != nil {
			// for config server
			secrets = append(secrets, mongodb.MustCertSecretName(api.MongoDBServerCert, mongodb.ConfigSvrNodeName()))
			// for shards
			for i := 0; i < int(mongodb.Spec.ShardTopology.Shard.Shards); i++ {
				secrets = append(secrets, mongodb.MustCertSecretName(api.MongoDBServerCert, mongodb.ShardNodeName(int32(i))))
			}
			// for mongos
			secrets = append(secrets, mongodb.MustCertSecretName(api.MongoDBServerCert, mongodb.MongosNodeName()))
		} else {
			// ReplicaSet or Standalone
			secrets = append(secrets, mongodb.MustCertSecretName(api.MongoDBServerCert, ""))
		}
		// for stash/user
		secrets = append(secrets, mongodb.MustCertSecretName(api.MongoDBClientCert, ""))
		// for prometheus exporter
		secrets = append(secrets, mongodb.MustCertSecretName(api.MongoDBMetricsExporterCert, ""))

		ok, err := dynamic_util.ResourcesExists(
			c.DynamicClient,
			core.SchemeGroupVersion.WithResource("secrets"),
			mongodb.Namespace,
			secrets...,
		)
		if err != nil {
			return err
		}
		if !ok {
			log.Infof("wait for all certificate secrets for MongoDB %s/%s", mongodb.Namespace, mongodb.Name)
			return nil
		}
	}

	// ensure database StatefulSet
	vt2, err := c.ensureMongoDBNode(mongodb)
	if err != nil {
		return err
	}

	if vt1 == kutil.VerbCreated && vt2 == kutil.VerbCreated {
		c.recorder.Event(
			mongodb,
			core.EventTypeNormal,
			eventer.EventReasonSuccessful,
			"Successfully created MongoDB",
		)
	} else if vt1 == kutil.VerbPatched || vt2 == kutil.VerbPatched {
		c.recorder.Event(
			mongodb,
			core.EventTypeNormal,
			eventer.EventReasonSuccessful,
			"Successfully patched MongoDB",
		)
	}

	// ensure appbinding before ensuring Restic scheduler and restore
	_, err = c.ensureAppBinding(mongodb)
	if err != nil {
		log.Errorln(err)
		return err
	}

	if _, err := meta_util.GetString(mongodb.Annotations, api.AnnotationInitialized); err == kutil.ErrNotFound &&
		mongodb.Spec.Init != nil && mongodb.Spec.Init.StashRestoreSession != nil {

		if mongodb.Status.Phase == api.DatabasePhaseInitializing {
			return nil
		}

		// add phase that database is being initialized
		mg, err := util.UpdateMongoDBStatus(context.TODO(), c.ExtClient.KubedbV1alpha1(), mongodb.ObjectMeta, func(in *api.MongoDBStatus) *api.MongoDBStatus {
			in.Phase = api.DatabasePhaseInitializing
			return in
		}, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		mongodb.Status = mg.Status

		init := mongodb.Spec.Init
		if init.StashRestoreSession != nil {
			log.Debugf("MongoDB %v/%v is waiting for restoreSession to be succeeded", mongodb.Namespace, mongodb.Name)
			return nil
		}
	}

	mg, err := util.UpdateMongoDBStatus(context.TODO(), c.ExtClient.KubedbV1alpha1(), mongodb.ObjectMeta, func(in *api.MongoDBStatus) *api.MongoDBStatus {
		in.Phase = api.DatabasePhaseRunning
		in.ObservedGeneration = mongodb.Generation
		return in
	}, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	mongodb.Status = mg.Status

	// ensure StatsService for desired monitoring
	if _, err := c.ensureStatsService(mongodb); err != nil {
		c.recorder.Eventf(
			mongodb,
			core.EventTypeWarning,
			eventer.EventReasonFailedToCreate,
			"Failed to manage monitoring system. Reason: %v",
			err,
		)
		log.Errorf("failed to manage monitoring system. Reason: %v", err)
		return nil
	}

	if err := c.manageMonitor(mongodb); err != nil {
		c.recorder.Eventf(
			mongodb,
			core.EventTypeWarning,
			eventer.EventReasonFailedToCreate,
			"Failed to manage monitoring system. Reason: %v",
			err,
		)
		log.Errorf("failed to manage monitoring system. Reason: %v", err)
		return nil
	}

	return nil
}

func (c *Controller) halt(db *api.MongoDB) error {
	if db.Spec.Halted && db.Spec.TerminationPolicy != api.TerminationPolicyHalt {
		return errors.New("can't halt db. 'spec.terminationPolicy' is not 'Halt'")
	}
	log.Infof("Halting MongoDB %v/%v", db.Namespace, db.Name)
	if err := c.haltDatabase(db); err != nil {
		return err
	}
	if err := c.waitUntilPaused(db); err != nil {
		return err
	}
	log.Infof("update status of MongoDB %v/%v to Halted.", db.Namespace, db.Name)
	if _, err := util.UpdateMongoDBStatus(context.TODO(), c.ExtClient.KubedbV1alpha1(), db.ObjectMeta, func(in *api.MongoDBStatus) *api.MongoDBStatus {
		in.Phase = api.DatabasePhaseHalted
		in.ObservedGeneration = db.Generation
		return in
	}, metav1.UpdateOptions{}); err != nil {
		return err
	}
	return nil
}

func (c *Controller) terminate(db *api.MongoDB) error {
	owner := metav1.NewControllerRef(db, api.SchemeGroupVersion.WithKind(api.ResourceKindMongoDB))

	// If TerminationPolicy is "halt", keep PVCs and Secrets intact.
	// TerminationPolicyPause is deprecated and will be removed in future.
	if db.Spec.TerminationPolicy == api.TerminationPolicyHalt || db.Spec.TerminationPolicy == api.TerminationPolicyPause {
		if err := c.removeOwnerReferenceFromOffshoots(db); err != nil {
			return err
		}
	} else {
		// If TerminationPolicy is "wipeOut", delete everything (ie, PVCs,Secrets,Snapshots).
		// If TerminationPolicy is "delete", delete PVCs and keep snapshots,secrets intact.
		// In both these cases, don't create dormantdatabase
		if err := c.setOwnerReferenceToOffshoots(db, owner); err != nil {
			return err
		}
	}

	if db.Spec.Monitor != nil {
		if err := c.deleteMonitor(db); err != nil {
			log.Errorln(err)
			return nil
		}
	}
	return nil
}

func (c *Controller) setOwnerReferenceToOffshoots(db *api.MongoDB, owner *metav1.OwnerReference) error {
	selector := labels.SelectorFromSet(db.OffshootSelectors())

	// If TerminationPolicy is "wipeOut", delete snapshots and secrets,
	// else, keep it intact.
	if db.Spec.TerminationPolicy == api.TerminationPolicyWipeOut {
		// wipeOut restoreSession
		if err := c.wipeOutDatabase(db.ObjectMeta, db.Spec.GetSecrets(), owner); err != nil {
			return errors.Wrap(err, "error in wiping out database.")
		}
	} else {
		// Make sure secret's ownerreference is removed.
		if err := dynamic_util.RemoveOwnerReferenceForItems(
			context.TODO(),
			c.DynamicClient,
			core.SchemeGroupVersion.WithResource("secrets"),
			db.Namespace,
			db.Spec.GetSecrets(),
			db); err != nil {
			return err
		}
	}
	// delete PVC for both "wipeOut" and "delete" TerminationPolicy.
	return dynamic_util.EnsureOwnerReferenceForSelector(
		context.TODO(),
		c.DynamicClient,
		core.SchemeGroupVersion.WithResource("persistentvolumeclaims"),
		db.Namespace,
		selector,
		owner)
}

func (c *Controller) removeOwnerReferenceFromOffshoots(db *api.MongoDB) error {
	// First, Get LabelSelector for Other Components
	labelSelector := labels.SelectorFromSet(db.OffshootSelectors())

	if err := dynamic_util.RemoveOwnerReferenceForSelector(
		context.TODO(),
		c.DynamicClient,
		core.SchemeGroupVersion.WithResource("persistentvolumeclaims"),
		db.Namespace,
		labelSelector,
		db); err != nil {
		return err
	}
	if err := dynamic_util.RemoveOwnerReferenceForItems(
		context.TODO(),
		c.DynamicClient,
		core.SchemeGroupVersion.WithResource("secrets"),
		db.Namespace,
		db.Spec.GetSecrets(),
		db); err != nil {
		return err
	}
	return nil
}
