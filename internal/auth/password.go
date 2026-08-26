package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// passwordAlgorithm is the value persisted in auth.json next to the hash so a
// future migration can tell which KDF produced it.
const passwordAlgorithm = "argon2id"

const (
	passwordSaltSize   = 16
	passwordKeySize    = 32
	passwordMemoryKiB  = 19456
	passwordTimeCost   = 2
	passwordThreads    = 1
	minPasswordLength  = 12
	maxPasswordLength  = 128
)

var ErrWeakPassword = errors.New("password must be between 12 and 128 characters")

func validatePassword(password string) (string, error) {
	length := len([]rune(password))
	if length < minPasswordLength || length > maxPasswordLength || strings.TrimSpace(password) == "" {
		return "", ErrWeakPassword
	}
	return password, nil
}

// HashPassword derives the at-rest representation of a dashboard password in
// the standardized PHC string format, including the per-hash random salt.
func HashPassword(password string) (string, error) {
	password, err := validatePassword(password)
	if err != nil {
		return "", err
	}
	salt := make([]byte, passwordSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, passwordTimeCost, passwordMemoryKiB, passwordThreads, passwordKeySize)
	return fmt.Sprintf("$%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		passwordAlgorithm, argon2.Version,
		passwordMemoryKiB, passwordTimeCost, passwordThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// verifyPasswordHash reports whether the candidate matches a stored PHC
// string. Unparseable or mismatched parameters always fail.
func verifyPasswordHash(stored, candidate string) bool {
	fields := strings.Split(stored, "$")
	if len(fields) != 6 || fields[0] != "" || fields[1] != passwordAlgorithm {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(fields[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(fields[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil || len(salt) == 0 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil || len(expected) == 0 {
		return false
	}
	actual := argon2.IDKey([]byte(candidate), salt, timeCost, memory, threads, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}
