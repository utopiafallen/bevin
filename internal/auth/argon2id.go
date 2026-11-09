package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (OWASP-recommended floor).
const (
	argonVersion   = argon2.Version // 19
	argonTime      = 2             // iterations
	argonMemoryKiB = 64 * 1024     // 64 MiB
	argonThreads   = 4
	argonKeyLen    = 32
	argonSaltLen   = 16
)

// hashArgon2id returns a PHC-format argon2id encoded string.
func hashArgon2id(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argonVersion, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// verifyArgon2id checks a password against a PHC argon2id encoded string
// (constant-time). Returns false on any malformed input.
func verifyArgon2id(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	// parts: ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, key]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var memory, timeIt, threads int
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeIt, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return false
	}
	want := argon2.IDKey([]byte(password), salt, uint32(timeIt), uint32(memory), uint8(threads), uint32(len(key)))
	return subtle.ConstantTimeCompare(want, key) == 1
}