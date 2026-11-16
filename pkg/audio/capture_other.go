//go:build !darwin && !windows && !linux

package audio

import (
	"context"
	"errors"
	"io"
)

func Capture(ctx context.Context, w io.Writer) error {
	return errors.ErrUnsupported
}
