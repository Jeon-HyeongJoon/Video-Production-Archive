//go:build !windows

package windowsvolume

func NewSystemObserver() (*Observer, error) {
	return nil, ErrObservationFailed
}
