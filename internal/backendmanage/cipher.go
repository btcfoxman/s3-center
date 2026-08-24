package backendmanage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

const envelopeVersion byte = 1

type definitionCipher struct {
	aead cipher.AEAD
}

func newDefinitionCipher(masterKey string) (*definitionCipher, error) {
	key := sha256.Sum256([]byte("s3-center/managed-backends/v1/" + masterKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("create managed backend cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create managed backend AEAD: %w", err)
	}
	return &definitionCipher{aead: aead}, nil
}

func (c *definitionCipher) seal(name string, definition Definition) ([]byte, error) {
	plain, err := json.Marshal(definition)
	if err != nil {
		return nil, fmt.Errorf("encode managed backend %q: %w", name, err)
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("create managed backend nonce: %w", err)
	}
	result := make([]byte, 1, 1+len(nonce)+len(plain)+c.aead.Overhead())
	result[0] = envelopeVersion
	result = append(result, nonce...)
	result = c.aead.Seal(result, nonce, plain, []byte(name))
	return result, nil
}

func (c *definitionCipher) open(name string, encrypted []byte) (Definition, error) {
	var definition Definition
	if len(encrypted) < 1+c.aead.NonceSize()+c.aead.Overhead() || encrypted[0] != envelopeVersion {
		return definition, fmt.Errorf("managed backend %q has an unsupported encrypted payload", name)
	}
	nonceEnd := 1 + c.aead.NonceSize()
	plain, err := c.aead.Open(nil, encrypted[1:nonceEnd], encrypted[nonceEnd:], []byte(name))
	if err != nil {
		return definition, fmt.Errorf("decrypt managed backend %q: %w", name, err)
	}
	if err := json.Unmarshal(plain, &definition); err != nil {
		return definition, fmt.Errorf("decode managed backend %q: %w", name, err)
	}
	return definition, nil
}
