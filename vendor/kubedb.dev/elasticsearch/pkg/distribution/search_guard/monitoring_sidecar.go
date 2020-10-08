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

package search_guard

import (
	"fmt"
	"path/filepath"

	api "kubedb.dev/apimachinery/apis/kubedb/v1alpha2"
	certlib "kubedb.dev/elasticsearch/pkg/lib/cert"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	core_util "kmodules.xyz/client-go/core/v1"
	meta_util "kmodules.xyz/client-go/meta"
	mona "kmodules.xyz/monitoring-agent-api/api/v1"
)

func (es *Elasticsearch) upsertMonitoringContainer(containers []corev1.Container) ([]corev1.Container, error) {
	if es.elasticsearch.Spec.Monitor != nil && es.elasticsearch.Spec.Monitor.Agent.Vendor() == mona.VendorPrometheus {
		var uri string
		if es.elasticsearch.Spec.DisableSecurity {
			uri = fmt.Sprintf("%s://localhost:%d", es.elasticsearch.GetConnectionScheme(), api.ElasticsearchRestPort)
		} else {
			uri = fmt.Sprintf("%s://$(DB_USER):$(DB_PASSWORD)@localhost:%d", es.elasticsearch.GetConnectionScheme(), api.ElasticsearchRestPort)
		}

		container := corev1.Container{
			Name: "exporter",
			Args: append([]string{
				fmt.Sprintf("--es.uri=%s", uri),
				fmt.Sprintf("--web.listen-address=:%d", func() int32 {
					if es.elasticsearch.Spec.Monitor.Prometheus != nil {
						return es.elasticsearch.Spec.Monitor.Prometheus.Exporter.Port
					}
					return int32(mona.PrometheusExporterPortNumber)
				}()),
				fmt.Sprintf("--web.telemetry-path=%s", es.elasticsearch.StatsService().Path()),
			}, es.elasticsearch.Spec.Monitor.Prometheus.Exporter.Args...),
			Image:           es.esVersion.Spec.Exporter.Image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Ports: []corev1.ContainerPort{
				{
					Name:     mona.PrometheusExporterPortName,
					Protocol: corev1.ProtocolTCP,
					ContainerPort: func() int32 {
						if es.elasticsearch.Spec.Monitor.Prometheus != nil {
							return es.elasticsearch.Spec.Monitor.Prometheus.Exporter.Port
						}
						return int32(mona.PrometheusExporterPortNumber)
					}(),
				},
			},
			Env:             es.elasticsearch.Spec.Monitor.Prometheus.Exporter.Env,
			Resources:       es.elasticsearch.Spec.Monitor.Prometheus.Exporter.Resources,
			SecurityContext: es.elasticsearch.Spec.Monitor.Prometheus.Exporter.SecurityContext,
		}

		if !es.elasticsearch.Spec.DisableSecurity {
			sName := es.elasticsearch.UserCredSecretName(string(api.ElasticsearchInternalUserMetricsExporter))
			_, err := es.getSecret(sName, es.elasticsearch.Namespace)
			if err != nil {
				return nil, errors.Wrap(err, "failed to get metrics-exporter-cred secret")
			}

			envList := []corev1.EnvVar{
				{
					Name: "DB_USER",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: sName,
							},
							Key: corev1.BasicAuthUsernameKey,
						},
					},
				},
				{
					Name: "DB_PASSWORD",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: sName,
							},
							Key: corev1.BasicAuthPasswordKey,
						},
					},
				},
			}
			container.Env = core_util.UpsertEnvVars(container.Env, envList...)
		}

		if es.elasticsearch.Spec.EnableSSL {
			certVolumeMount := corev1.VolumeMount{
				Name:      es.elasticsearch.CertSecretVolumeName(api.ElasticsearchMetricsExporterCert),
				MountPath: ExporterCertDir,
			}
			container.VolumeMounts = core_util.UpsertVolumeMount(container.VolumeMounts, certVolumeMount)

			esCaFlags := []string{
				"--es.ca=" + filepath.Join(ExporterCertDir, certlib.CACert),
				"--es.client-cert=" + filepath.Join(ExporterCertDir, certlib.TLSCert),
				"--es.client-private-key=" + filepath.Join(ExporterCertDir, certlib.TLSKey),
			}

			// upsert container Args
			container.Args = meta_util.UpsertArgumentList(container.Args, esCaFlags)
		}
		containers = core_util.UpsertContainer(containers, container)
	} else {
		return nil, errors.New("unknown monitoring vendor")
	}

	return containers, nil
}
