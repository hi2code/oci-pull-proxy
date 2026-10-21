package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

func Authorized(r *http.Request, username, password string) bool {
	u, p, ok := r.BasicAuth()
	a, b := sha256.Sum256([]byte(u)), sha256.Sum256([]byte(username))
	c, d := sha256.Sum256([]byte(p)), sha256.Sum256([]byte(password))
	return ok && password != "" && subtle.ConstantTimeCompare(a[:], b[:]) == 1 && subtle.ConstantTimeCompare(c[:], d[:]) == 1
}
