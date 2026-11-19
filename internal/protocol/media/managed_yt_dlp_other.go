//go:build !darwin

package media

func NewManagedYtDlp(storageDir, socket string) ManagedYtDlp { return nil }
