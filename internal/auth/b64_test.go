package auth

import "encoding/base64"

func decodeB64URL(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
