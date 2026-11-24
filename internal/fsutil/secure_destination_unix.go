//go:build unix

package fsutil

// NewPrivateDestinationFileSystem uses the existing Unix secure-directory checks.
func NewPrivateDestinationFileSystem(string) (OSFileSystem, error) {
	return OSFileSystem{}, nil
}

func (fsys OSFileSystem) WithPrivateDestination(string) (OSFileSystem, error) {
	return fsys, nil
}
