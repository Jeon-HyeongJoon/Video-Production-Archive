package ossecret

import "errors"

var (
	ErrSecretUnavailable   = errors.New("OS secret is unavailable")
	ErrUnsupportedPlatform = errors.New("OS secret store is unsupported on this platform")
)
