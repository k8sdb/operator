package controller

import (
	"fmt"

	"github.com/appscode/go/crypto/rand"
	"github.com/appscode/go/log"
	api "github.com/kubedb/apimachinery/apis/kubedb/v1alpha1"
	"github.com/kubedb/apimachinery/pkg/docker"
	"github.com/kubedb/apimachinery/pkg/storage"
	amv "github.com/kubedb/apimachinery/pkg/validator"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	SnapshotProcess_Backup  = "backup"
	snapshotType_DumpBackup = "dump-backup"
)

func (c *Controller) ValidateSnapshot(snapshot *api.Snapshot) error {
	// Database name can't empty
	databaseName := snapshot.Spec.DatabaseName
	if databaseName == "" {
		return fmt.Errorf(`object 'DatabaseName' is missing in '%v'`, snapshot.Spec)
	}

	if err := docker.CheckDockerImageVersion(docker.ImageElasticdump, c.opt.ElasticDumpTag); err != nil {
		return fmt.Errorf(`image %v:%v not found`, docker.ImageElasticdump, c.opt.ElasticDumpTag)
	}

	if _, err := c.ExtClient.Elasticsearchs(snapshot.Namespace).Get(databaseName, metav1.GetOptions{}); err != nil {
		return err
	}

	return amv.ValidateSnapshotSpec(c.Client, snapshot.Spec.SnapshotStorageSpec, snapshot.Namespace)
}

func (c *Controller) GetDatabase(snapshot *api.Snapshot) (runtime.Object, error) {
	elasticsearch, err := c.ExtClient.Elasticsearchs(snapshot.Namespace).Get(snapshot.Spec.DatabaseName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return elasticsearch, nil
}

func (c *Controller) GetSnapshotter(snapshot *api.Snapshot) (*batch.Job, error) {
	databaseName := snapshot.Spec.DatabaseName
	jobName := rand.WithUniqSuffix(snapshot.OffshootName())
	jobLabel := map[string]string{
		api.LabelDatabaseName: databaseName,
		api.LabelJobType:      SnapshotProcess_Backup,
	}
	backupSpec := snapshot.Spec.SnapshotStorageSpec
	bucket, err := backupSpec.Container()
	if err != nil {
		return nil, err
	}
	elasticsearch, err := c.ExtClient.Elasticsearchs(snapshot.Namespace).Get(databaseName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	// Get PersistentVolume object for Backup Util pod.
	persistentVolume, err := c.getVolumeForSnapshot(elasticsearch.Spec.Storage, jobName, snapshot.Namespace)
	if err != nil {
		return nil, err
	}

	// Folder name inside Cloud bucket where backup will be uploaded
	folderName, _ := snapshot.Location()
	job := &batch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:   jobName,
			Labels: jobLabel,
		},
		Spec: batch.JobSpec{
			Template: core.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: jobLabel,
				},
				Spec: core.PodSpec{
					Containers: []core.Container{
						{
							Name:  SnapshotProcess_Backup,
							Image: docker.ImageElasticdump + ":" + c.opt.ElasticDumpTag,
							Args: []string{
								fmt.Sprintf(`--process=%s`, SnapshotProcess_Backup),
								fmt.Sprintf(`--host=%s`, databaseName),
								fmt.Sprintf(`--bucket=%s`, bucket),
								fmt.Sprintf(`--folder=%s`, folderName),
								fmt.Sprintf(`--snapshot=%s`, snapshot.Name),
							},
							Env: []core.EnvVar{
								{
									Name:  "USERNAME",
									Value: "readall",
								},
								{
									Name: "PASSWORD",
									ValueFrom: &core.EnvVarSource{
										SecretKeyRef: &core.SecretKeySelector{
											LocalObjectReference: core.LocalObjectReference{
												Name: elasticsearch.Spec.DatabaseSecret.SecretName,
											},
											Key: "READALL_PASSWORD",
										},
									},
								},
							},
							Resources: snapshot.Spec.Resources,
							VolumeMounts: []core.VolumeMount{
								{
									Name:      persistentVolume.Name,
									MountPath: "/var/" + snapshotType_DumpBackup + "/",
								},
								{
									Name:      "osmconfig",
									MountPath: storage.SecretMountPath,
									ReadOnly:  true,
								},
								{
									Name:      "certs",
									MountPath: "/certs",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []core.Volume{
						{
							Name:         persistentVolume.Name,
							VolumeSource: persistentVolume.VolumeSource,
						},
						{
							Name: "osmconfig",
							VolumeSource: core.VolumeSource{
								Secret: &core.SecretVolumeSource{
									SecretName: snapshot.OSMSecretName(),
								},
							},
						},
						{
							Name: "certs",
							VolumeSource: core.VolumeSource{
								Secret: &core.SecretVolumeSource{
									SecretName: elasticsearch.Spec.CertificateSecret.SecretName,
									Items: []core.KeyToPath{
										{
											Key:  "ca.pem",
											Path: "ca.pem",
										},
										{
											Key:  "client-key.pem",
											Path: "client-key.pem",
										},
										{
											Key:  "client.pem",
											Path: "client.pem",
										},
									},
								},
							},
						},
					},
					RestartPolicy: core.RestartPolicyNever,
				},
			},
		},
	}

	if snapshot.Spec.SnapshotStorageSpec.Local != nil {
		job.Spec.Template.Spec.Containers[0].VolumeMounts = append(job.Spec.Template.Spec.Containers[0].VolumeMounts, core.VolumeMount{
			Name:      "local",
			MountPath: snapshot.Spec.SnapshotStorageSpec.Local.Path,
		})
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, core.Volume{
			Name:         "local",
			VolumeSource: snapshot.Spec.SnapshotStorageSpec.Local.VolumeSource,
		})
	}
	return job, nil
}

func (c *Controller) WipeOutSnapshot(snapshot *api.Snapshot) error {
	return c.DeleteSnapshotData(snapshot)
}

func (c *Controller) getVolumeForSnapshot(pvcSpec *core.PersistentVolumeClaimSpec, jobName, namespace string) (*core.Volume, error) {
	volume := &core.Volume{
		Name: "util-volume",
	}
	if pvcSpec != nil {
		if len(pvcSpec.AccessModes) == 0 {
			pvcSpec.AccessModes = []core.PersistentVolumeAccessMode{
				core.ReadWriteOnce,
			}
			log.Infof(`Using "%v" as AccessModes in "%v"`, core.ReadWriteOnce, *pvcSpec)
		}

		claim := &core.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName,
				Namespace: namespace,
				Annotations: map[string]string{
					"volume.beta.kubernetes.io/storage-class": *pvcSpec.StorageClassName,
				},
			},
			Spec: *pvcSpec,
		}

		if _, err := c.Client.CoreV1().PersistentVolumeClaims(claim.Namespace).Create(claim); err != nil {
			return nil, err
		}

		volume.PersistentVolumeClaim = &core.PersistentVolumeClaimVolumeSource{
			ClaimName: claim.Name,
		}
	} else {
		volume.EmptyDir = &core.EmptyDirVolumeSource{}
	}
	return volume, nil
}
