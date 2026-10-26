//go:build !windows

package ossecret

type Resolver struct{}

func New() *Resolver { return &Resolver{} }

func (*Resolver) Resolve(string) (string, error) {
	return "", ErrUnsupportedPlatform
}
