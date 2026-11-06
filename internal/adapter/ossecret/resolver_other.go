//go:build !windows && !darwin

package ossecret

type Resolver struct{}

func New() *Resolver { return &Resolver{} }

func (*Resolver) Resolve(string) (string, error) {
	return "", ErrUnsupportedPlatform
}

func ProvisionDPAPI(string, string) error { return ErrUnsupportedPlatform }

func ProvisionKeychain(string, string) error { return ErrUnsupportedPlatform }

func DeleteKeychain(string) error { return ErrUnsupportedPlatform }
