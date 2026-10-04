//go:build !linux

package launcher

import (
	"context"
	"errors"
	"log/slog"
)

// Windows can test pure verification/storage logic, but no production TCP or
// unverified process fallback replaces the private Linux launcher transport.
func Run(context.Context, Config, *slog.Logger) error {
	return errors.New("the standalone container launcher requires Linux")
}
