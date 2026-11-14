package keystore

import "context"

func SealProviderKey(ctx context.Context, kms KMSClient, plaintextKey []byte) (ciphertext, nonce, encryptedDEK []byte, kmsKeyID string, err error) {
	dek := GenerateDEK()
	defer Zero(dek)

	ciphertext, nonce, err = EncryptWithDEK(dek, plaintextKey)

	encryptedDEK, kmsKeyID, err = kms.Encrypt(ctx, dek)
	if err != nil {
		return nil, nil, nil, "", err
	}

	return ciphertext, nonce, encryptedDEK, kmsKeyID, nil
}

func OpenProviderKey(ctx context.Context, kms KMSClient, ciphertext, nonce, encryptedDEK []byte, kmsKeyID string) ([]byte, error) {
	dek, err := kms.Decrypt(ctx, encryptedDEK, kmsKeyID)
	if err != nil {
		return nil, err
	}
	defer Zero(dek)

	return DecryptWithDEK(dek, nonce, ciphertext)
}
