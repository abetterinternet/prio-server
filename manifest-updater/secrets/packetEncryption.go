package secrets

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"fmt"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"
)

const (
	packetDecryptionKeyMaxAge = 20 * 24 * time.Hour
)

func (k *Kube) validateAndUpdatePacketEncryptionKey(secret *corev1.Secret) (*PrioKey, error) {
	data := secret.Data

	creation := secret.GetCreationTimestamp()
	since := time.Since(creation.Time)

	expired := since > packetDecryptionKeyMaxAge

	_, ok := data[secretKeyMap]
	if ok && !expired {
		return nil, nil
	}
	k.log.
		WithField("Secret existence: ", ok).
		WithField("Expiration: ", expired).
		Info("Secret value didn't exist, or secret expired, we're going to assume the secret is invalid and make a new one")

	err := k.deletePacketEncryptionSecret(secret)

	if err != nil {
		return nil, fmt.Errorf("unable to delete existing secret: %w", err)
	}

	key, err := k.createAndStorePacketEncryptionKey()

	if err != nil {
		k.log.WithError(err).Errorln("Secret creation after deletion failed! This is going to cause problems :(")
		return nil, fmt.Errorf("unable to create secret after deleting: %w", err)
	}

	return key, nil

}

func (k *Kube) deletePacketEncryptionSecret(secret *corev1.Secret) error {
	sApi := k.client.CoreV1().Secrets(k.namespace)

	var deletion int64
	deletion = 0

	if err := sApi.Delete(context.Background(), secret.Name, v1.DeleteOptions{GracePeriodSeconds: &deletion}); err != nil {
		return errors.Wrap(err, "unable to delete")
	}

	return nil
}

func (k *Kube) createAndStorePacketEncryptionKey() (*PrioKey, error) {
	key, err := NewPrioKey()

	if err != nil {
		return nil, fmt.Errorf("unable to create a packet encryption key: %w", err)
	}

	immutable := true

	secret := corev1.Secret{
		ObjectMeta: v1.ObjectMeta{
			Name:      packetDecryptionKeyName,
			Namespace: k.namespace,
		},
		Immutable: &immutable,

		StringData: map[string]string{
			secretKeyMap: base64.StdEncoding.EncodeToString(key.MarshallX962UncompressedPrivateKey()),
		},
	}

	sApi := k.client.CoreV1().Secrets(k.namespace)
	created, err := sApi.Create(context.Background(), &secret, v1.CreateOptions{})

	if err != nil {
		return nil, errors.Wrap(err, "failed to store secret")
	}

	key.KubeIdentifier = created.Name

	return key, err
}

// marshalX962UncompressedPrivateKey encodes a P-256 private key into the format
// expected by libprio-rs encrypt::PrivateKey, which is the X9.62 uncompressed
// public key concatenated with the secret scalar.
func marshalX962UncompressedPrivateKey(ecdsaKey *ecdsa.PrivateKey) ([]byte, error) {
	marshaledPublicKey := elliptic.Marshal(elliptic.P256(), ecdsaKey.PublicKey.X, ecdsaKey.PublicKey.Y)
	return append(marshaledPublicKey, ecdsaKey.D.Bytes()...), nil
}
