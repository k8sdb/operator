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
package controller

import (
	api "kubedb.dev/apimachinery/apis/kubedb/v1alpha1"

	"github.com/appscode/go/log"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	kutil "kmodules.xyz/client-go"
	core_util "kmodules.xyz/client-go/core/v1"
	dynamic_util "kmodules.xyz/client-go/dynamic"
)

func (c *Controller) waitUntilPaused(db *api.PerconaXtraDB) error {
	log.Infof("waiting for pods for PerconaXtraDB %v/%v to be deleted\n", db.Namespace, db.Name)
	if err := core_util.WaitUntilPodDeletedBySelector(c.Client, db.Namespace, metav1.SetAsLabelSelector(db.OffshootSelectors())); err != nil {
		return err
	}

	log.Infof("waiting for services for PerconaXtraDB %v/%v to be deleted\n", db.Namespace, db.Name)
	if err := core_util.WaitUntilServiceDeletedBySelector(c.Client, db.Namespace, metav1.SetAsLabelSelector(db.OffshootSelectors())); err != nil {
		return err
	}

	if err := c.waitUntilRBACStuffDeleted(db); err != nil {
		return err
	}

	if err := c.waitUntilStatefulSetsDeleted(db); err != nil {
		return err
	}

	if err := c.waitUntilDeploymentsDeleted(db); err != nil {
		return err
	}

	return nil
}

func (c *Controller) waitUntilRBACStuffDeleted(px *api.PerconaXtraDB) error {
	log.Infof("waiting for RBACs for PerconaXtraDB %v/%v to be deleted\n", px.Namespace, px.Name)
	// Delete ServiceAccount
	if err := core_util.WaitUntillServiceAccountDeleted(c.Client, px.ObjectMeta); err != nil {
		return err
	}
	return nil
}

func (c *Controller) waitUntilStatefulSetsDeleted(db *api.PerconaXtraDB) error {
	log.Infof("waiting for statefulsets for PerconaXtraDB %v/%v to be deleted\n", db.Namespace, db.Name)
	return wait.PollImmediate(kutil.RetryInterval, kutil.GCTimeout, func() (bool, error) {
		if sts, err := c.Client.AppsV1().StatefulSets(db.Namespace).List(metav1.ListOptions{LabelSelector: labels.SelectorFromSet(db.OffshootSelectors()).String()}); err != nil && kerr.IsNotFound(err) || len(sts.Items) == 0 {
			return true, nil
		}
		return false, nil
	})
}

func (c *Controller) waitUntilDeploymentsDeleted(db *api.PerconaXtraDB) error {
	log.Infof("waiting for deployments for PerconaXtraDB %v/%v to be deleted\n", db.Namespace, db.Name)
	return wait.PollImmediate(kutil.RetryInterval, kutil.GCTimeout, func() (bool, error) {
		if deploys, err := c.Client.AppsV1().Deployments(db.Namespace).List(metav1.ListOptions{LabelSelector: labels.SelectorFromSet(db.OffshootSelectors()).String()}); err != nil && kerr.IsNotFound(err) || len(deploys.Items) == 0 {
			return true, nil
		}
		return false, nil
	})
}

// haltDatabase keeps PVC and secrets and deletes rest of the resources generated by kubedb
func (c *Controller) haltDatabase(db *api.PerconaXtraDB) error {
	labelSelector := labels.SelectorFromSet(db.OffshootSelectors()).String()
	policy := metav1.DeletePropagationForeground

	// delete appbinding
	log.Infof("deleting AppBindings of PerconaXtraDB %v/%v.", db.Namespace, db.Name)
	if err := c.AppCatalogClient.
		AppcatalogV1alpha1().
		AppBindings(db.Namespace).
		DeleteCollection(
			&metav1.DeleteOptions{PropagationPolicy: &policy},
			metav1.ListOptions{LabelSelector: labelSelector},
		); err != nil {
		return err
	}

	// delete PDB
	log.Infof("deleting PodDisruptionBudget of PerconaXtraDB %v/%v.", db.Namespace, db.Name)
	if err := c.Client.
		PolicyV1beta1().
		PodDisruptionBudgets(db.Namespace).
		DeleteCollection(
			&metav1.DeleteOptions{PropagationPolicy: &policy},
			metav1.ListOptions{LabelSelector: labelSelector},
		); err != nil {
		return err
	}

	// delete sts collection offshoot labels
	log.Infof("deleting StatefulSets of PerconaXtraDB %v/%v.", db.Namespace, db.Name)
	if err := c.Client.
		AppsV1().
		StatefulSets(db.Namespace).
		DeleteCollection(
			&metav1.DeleteOptions{PropagationPolicy: &policy},
			metav1.ListOptions{LabelSelector: labelSelector},
		); err != nil {
		return err
	}

	// delete deployment collection offshoot labels
	log.Infof("deleting Deployments of PerconaXtraDB %v/%v.", db.Namespace, db.Name)
	if err := c.Client.
		AppsV1().
		Deployments(db.Namespace).
		DeleteCollection(
			&metav1.DeleteOptions{PropagationPolicy: &policy},
			metav1.ListOptions{LabelSelector: labelSelector},
		); err != nil {
		return err
	}

	// delete rbacs: rolebinding, roles, serviceaccounts
	log.Infof("deleting RoleBindings of PerconaXtraDB %v/%v.", db.Namespace, db.Name)
	if err := c.Client.
		RbacV1().
		RoleBindings(db.Namespace).
		DeleteCollection(
			&metav1.DeleteOptions{PropagationPolicy: &policy},
			metav1.ListOptions{LabelSelector: labelSelector},
		); err != nil {
		return err
	}
	log.Infof("deleting Roles of PerconaXtraDB %v/%v.", db.Namespace, db.Name)
	if err := c.Client.
		RbacV1().
		Roles(db.Namespace).
		DeleteCollection(
			&metav1.DeleteOptions{PropagationPolicy: &policy},
			metav1.ListOptions{LabelSelector: labelSelector},
		); err != nil {
		return err
	}
	log.Infof("deleting ServiceAccounts of PerconaXtraDB %v/%v.", db.Namespace, db.Name)
	if err := c.Client.
		CoreV1().
		ServiceAccounts(db.Namespace).
		DeleteCollection(
			&metav1.DeleteOptions{PropagationPolicy: &policy},
			metav1.ListOptions{LabelSelector: labelSelector},
		); err != nil {
		return err
	}
	// delete services

	// service, stats service, gvr service
	log.Infof("deleting Services of PerconaXtraDB %v/%v.", db.Namespace, db.Name)
	svcs, err := c.Client.
		CoreV1().
		Services(db.Namespace).
		List(metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil && !kerr.IsNotFound(err) {
		return err
	}
	for _, svc := range svcs.Items {
		if err := c.Client.
			CoreV1().
			Services(db.Namespace).
			Delete(svc.Name, &metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil {
			return err
		}
	}

	// Delete monitoring resources
	log.Infof("deleting Monitoring resources of PerconaXtraDB %v/%v.", db.Namespace, db.Name)
	if db.Spec.Monitor != nil {
		if err := c.deleteMonitor(db); err != nil {
			log.Errorln(err)
			return nil
		}
	}
	return nil
}

// wipeOutDatabase is a generic function to call from WipeOutDatabase and percona-xtradb pause method.
func (c *Controller) wipeOutDatabase(meta metav1.ObjectMeta, secrets []string, owner *metav1.OwnerReference) error {
	secretUsed, err := c.secretsUsedByPeers(meta)
	if err != nil {
		return errors.Wrap(err, "error in getting used secret list")
	}
	unusedSecrets := sets.NewString(secrets...).Difference(secretUsed)
	return dynamic_util.EnsureOwnerReferenceForItems(
		c.DynamicClient,
		core.SchemeGroupVersion.WithResource("secrets"),
		meta.Namespace,
		unusedSecrets.List(),
		owner)
}

// isSecretUsed gets the DBList of same kind, then checks if our required secret is used by those.
// Similarly, isSecretUsed also checks for DomantDB of similar dbKind label.
func (c *Controller) secretsUsedByPeers(meta metav1.ObjectMeta) (sets.String, error) {
	secretUsed := sets.NewString()

	dbList, err := c.pxLister.PerconaXtraDBs(meta.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, px := range dbList {
		if px.Name != meta.Name {
			secretUsed.Insert(px.Spec.GetSecrets()...)
		}
	}
	return secretUsed, nil
}
