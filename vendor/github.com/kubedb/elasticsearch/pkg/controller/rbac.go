package controller

import (
	api "github.com/kubedb/apimachinery/apis/kubedb/v1alpha1"
	core "k8s.io/api/core/v1"
	policy_v1beta1 "k8s.io/api/policy/v1beta1"
	rbac "k8s.io/api/rbac/v1beta1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/reference"
	core_util "kmodules.xyz/client-go/core/v1"
	rbac_util "kmodules.xyz/client-go/rbac/v1beta1"
)

func (c *Controller) ensureRole(db *api.Elasticsearch, name string, pspName string) error {
	ref, rerr := reference.GetReference(clientsetscheme.Scheme, db)
	if rerr != nil {
		return rerr
	}

	// Create new Roles
	_, _, err := rbac_util.CreateOrPatchRole(
		c.Client,
		metav1.ObjectMeta{
			Name:      name,
			Namespace: db.Namespace,
		},
		func(in *rbac.Role) *rbac.Role {
			core_util.EnsureOwnerReference(&in.ObjectMeta, ref)
			in.Labels = db.OffshootLabels()
			in.Rules = []rbac.PolicyRule{}
			if pspName != "" {
				pspRule := rbac.PolicyRule{
					APIGroups:     []string{policy_v1beta1.GroupName},
					Resources:     []string{"podsecuritypolicies"},
					Verbs:         []string{"use"},
					ResourceNames: []string{pspName},
				}
				in.Rules = append(in.Rules, pspRule)
			}
			return in
		},
	)
	return err
}

func (c *Controller) createRoleBinding(db *api.Elasticsearch, name string) error {
	ref, rerr := reference.GetReference(clientsetscheme.Scheme, db)
	if rerr != nil {
		return rerr
	}
	// Ensure new RoleBindings
	_, _, err := rbac_util.CreateOrPatchRoleBinding(
		c.Client,
		metav1.ObjectMeta{
			Name:      name,
			Namespace: db.Namespace,
		},
		func(in *rbac.RoleBinding) *rbac.RoleBinding {
			core_util.EnsureOwnerReference(&in.ObjectMeta, ref)
			in.Labels = db.OffshootLabels()
			in.RoleRef = rbac.RoleRef{
				APIGroup: rbac.GroupName,
				Kind:     "Role",
				Name:     name,
			}
			in.Subjects = []rbac.Subject{
				{
					Kind:      rbac.ServiceAccountKind,
					Name:      name,
					Namespace: db.Namespace,
				},
			}
			return in
		},
	)
	return err
}

func (c *Controller) createServiceAccount(db *api.Elasticsearch, saName string) error {
	ref, rerr := reference.GetReference(clientsetscheme.Scheme, db)
	if rerr != nil {
		return rerr
	}
	// Create new ServiceAccount
	_, _, err := core_util.CreateOrPatchServiceAccount(
		c.Client,
		metav1.ObjectMeta{
			Name:      saName,
			Namespace: db.Namespace,
		},
		func(in *core.ServiceAccount) *core.ServiceAccount {
			core_util.EnsureOwnerReference(&in.ObjectMeta, ref)
			in.Labels = db.OffshootLabels()
			return in
		},
	)
	return err
}

func (c *Controller) getPolicyNames(db *api.Elasticsearch) (string, string, error) {
	dbVersion, err := c.ExtClient.CatalogV1alpha1().ElasticsearchVersions().Get(string(db.Spec.Version), metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	dbPolicyName := dbVersion.Spec.PodSecurityPolicies.DatabasePolicyName
	snapshotPolicyName := dbVersion.Spec.PodSecurityPolicies.SnapshotterPolicyName

	return dbPolicyName, snapshotPolicyName, nil
}

func (c *Controller) ensureRBACStuff(elasticsearch *api.Elasticsearch) error {
	dbPolicyName, snapshotPolicyName, err := c.getPolicyNames(elasticsearch)
	if err != nil {
		return err
	}

	// Create New Role
	if err := c.ensureRole(elasticsearch, elasticsearch.OffshootName(), dbPolicyName); err != nil {
		return err
	}

	// Create New RoleBinding
	if err := c.createRoleBinding(elasticsearch, elasticsearch.OffshootName()); err != nil {
		return err
	}

	// Create New ServiceAccount
	if err := c.createServiceAccount(elasticsearch, elasticsearch.OffshootName()); err != nil {
		if !kerr.IsAlreadyExists(err) {
			return err
		}
	}

	// Create New Role for Snapshot
	if err := c.ensureRole(elasticsearch, elasticsearch.SnapshotSAName(), snapshotPolicyName); err != nil {
		return err
	}

	// Create New RoleBinding for Snapshot
	if err := c.createRoleBinding(elasticsearch, elasticsearch.SnapshotSAName()); err != nil {
		return err
	}

	// Create New Snapshot ServiceAccount
	if err := c.createServiceAccount(elasticsearch, elasticsearch.SnapshotSAName()); err != nil {
		if !kerr.IsAlreadyExists(err) {
			return err
		}
	}

	return nil
}
