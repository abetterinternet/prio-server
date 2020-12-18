package tester

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/abetterinternet/prio-server/integration-tester/kubernetes"
	m "github.com/abetterinternet/prio-server/manifest-updater/manifest"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (t *Tester) Start() error {
	manifest, err := GetManifest(t.manifestFileUrl)
	if err != nil {
		return err
	}
	bsk, err := t.getValidBSK(manifest)
	if err != nil {
		return err
	}

	pdk, err := t.getValidPDK(manifest)
	if err != nil {
		return err
	}

	job := t.createJob(manifest, bsk, pdk)

	return kubernetes.ScheduleJob(t.namespace, job)
}

func (t *Tester) createJob(manifest *m.DataShareSpecificManifest, bsk, pdk *corev1.Secret) *batchv1.Job {
	trueP := true
	env := []corev1.EnvVar{
		{Name: "FACILITATOR_ECIES_PRIVATE_KEY",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: pdk.Name},
				Key:                  "secret_key",
			}}},
		{Name: "BATCH_SIGNING_PRIVATE_KEY",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: bsk.Name},
				Key:                  "secret_key",
			}}},
		{Name: "RUST_LOG", Value: "debug"},
		{Name: "RUST_BACKTRACE", Value: "1"},
		{Name: "AWS_ACCOUNT_ID", Value: t.awsAccountId},
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "integration-tester-facilitator",
			Labels: map[string]string{
				"type": "integration-tester",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:                "Never",
					ServiceAccountName:           t.serviceAccountName,
					AutomountServiceAccountToken: &trueP,
					Containers: []corev1.Container{
						{
							Name:  "integration-tester",
							Image: t.facilitatorImage,
							Env:   env,
							Args: []string{
								"--pushgateway", t.pushGateway,
								"generate-ingestion-sample",
								"--own-output", manifest.IngestionBucket,
								"--peer-output", manifest.PeerValidationBucket,
								"--peer-identity", t.peerIdentity,
								"--aggregation-id", "kittens-seen",
								// The various keys
								"--batch-signing-private-key-identifier", bsk.Name,
								"--pha-ecies-private-key", "BIl6j+J6dYttxALdjISDv6ZI4/VWVEhUzaS05LgrsfswmbLOgNt9HUC2E0w+9RqZx3XMkdEHBHfNuCSMpOwofVSq3TfyKwn0NrftKisKKVSaTOt5seJ67P5QL4hxgPWvxw==",
								"--packet-count", "10",
								// These parameters get recorded in Avro messages but otherwise
								// do not affect any system behavior, so the values don't matter.
								"--batch-start-time", "1000000000",
								"--batch-end-time", "1000000100",
								"--dimension", "123",
								"--epsilon", "0.23",
							},
						},
					},
				},
			},
		},
	}
}

func GetManifest(url string) (*m.DataShareSpecificManifest, error) {
	dsm := &m.DataShareSpecificManifest{}
	client := http.Client{Timeout: 10 * time.Second}

	r, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("unable to get %s: %v", err)
	}
	defer r.Body.Close()

	err = json.NewDecoder(r.Body).Decode(dsm)
	if err != nil {
		return nil, fmt.Errorf("unable to decode body %s: %v", r.Body, err)
	}
	return dsm, err
}

func (t *Tester) getValidPDK(manifest *m.DataShareSpecificManifest) (*corev1.Secret, error) {
	labelSelector := fmt.Sprintf("type=packet-decryption-key")
	secrets, err := kubernetes.GetSortedSecrets(t.namespace, labelSelector)
	if err != nil {
		return nil, err
	}
	secretMap := indexSecretsByName(secrets)
	for name := range manifest.PacketEncryptionKeyCSRs {
		val, ok := secretMap[name]

		if ok {
			return &val, nil
		}
	}
	return nil, fmt.Errorf("unable to find a suitable packet decryption key - manifest was: %s", manifest.PacketEncryptionKeyCSRs)
}

func (t *Tester) getValidBSK(manifest *m.DataShareSpecificManifest) (*corev1.Secret, error) {
	labelSelector := fmt.Sprintf("type=batch-signing-key,ingestor=%s", t.name)
	secrets, err := kubernetes.GetSortedSecrets(t.namespace, labelSelector)
	if err != nil {
		return nil, err
	}
	secretMap := indexSecretsByName(secrets)
	for name := range manifest.BatchSigningPublicKeys {
		val, ok := secretMap[name]

		if ok {
			return &val, nil
		}
	}
	return nil, fmt.Errorf("unable to find a suitable batch signing key - manifest was: %s", manifest.BatchSigningPublicKeys)
}

func indexSecretsByName(secrets []corev1.Secret) map[string]corev1.Secret {
	idx := map[string]corev1.Secret{}

	for _, secret := range secrets {
		idx[secret.Name] = secret
	}

	return idx
}
