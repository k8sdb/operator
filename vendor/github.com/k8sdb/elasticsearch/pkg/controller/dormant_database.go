package controller

import (
	"errors"

	"github.com/appscode/go/log"
	api "github.com/k8sdb/apimachinery/apis/kubedb/v1alpha1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

func (c *Controller) Exists(om *metav1.ObjectMeta) (bool, error) {
	if _, err := c.ExtClient.Elasticsearchs(om.Namespace).Get(om.Name, metav1.GetOptions{}); err != nil {
		if !kerr.IsNotFound(err) {
			return false, err
		}
		return false, nil
	}

	return true, nil
}

func (c *Controller) PauseDatabase(dormantDb *api.DormantDatabase) error {
	// Delete Service
	if err := c.DeleteService(dormantDb.Name, dormantDb.Namespace); err != nil {
		log.Errorln(err)
		return err
	}

	if err := c.DeleteStatefulSet(dormantDb.OffshootName(), dormantDb.Namespace); err != nil {
		log.Errorln(err)
		return err
	}

	elastic := &api.Elasticsearch{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dormantDb.OffshootName(),
			Namespace: dormantDb.Namespace,
		},
	}
	if err := c.deleteRBACStuff(elastic); err != nil {
		log.Errorln(err)
		return err
	}
	return nil
}

func (c *Controller) WipeOutDatabase(dormantDb *api.DormantDatabase) error {
	labelMap := map[string]string{
		api.LabelDatabaseName: dormantDb.Name,
		api.LabelDatabaseKind: api.ResourceKindElasticsearch,
	}

	labelSelector := labels.SelectorFromSet(labelMap)

	if err := c.DeleteSnapshots(dormantDb.Namespace, labelSelector); err != nil {
		log.Errorln(err)
		return err
	}

	if err := c.DeletePersistentVolumeClaims(dormantDb.Namespace, labelSelector); err != nil {
		log.Errorln(err)
		return err
	}
	return nil
}

func (c *Controller) ResumeDatabase(dormantDb *api.DormantDatabase) error {
	origin := dormantDb.Spec.Origin
	objectMeta := origin.ObjectMeta

	if origin.Spec.Elasticsearch.Init != nil {
		return errors.New("Do not support InitSpec in spec.origin.elasticsearch")
	}

	elastic := &api.Elasticsearch{
		ObjectMeta: metav1.ObjectMeta{
			Name:        objectMeta.Name,
			Namespace:   objectMeta.Namespace,
			Labels:      objectMeta.Labels,
			Annotations: objectMeta.Annotations,
		},
		Spec: *origin.Spec.Elasticsearch,
	}

	if elastic.Annotations == nil {
		elastic.Annotations = make(map[string]string)
	}

	for key, val := range dormantDb.Annotations {
		elastic.Annotations[key] = val
	}

	_, err := c.ExtClient.Elasticsearchs(elastic.Namespace).Create(elastic)
	return err
}
